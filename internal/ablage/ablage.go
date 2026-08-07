// Paket ablage ist die Datenbankseite: Schluessel, Toepfe, Kopfdaten.
//
// KEINE FUNKTION AUS DIESEM PAKET WIRD IM ANFRAGEWEG AUFGERUFEN. Das ist die Betriebsregel
// des Auftrags (§0) und der Grund, warum der Vorbau eine Datenbank ueberhaupt vertragen
// kann: gelesen wird beim Start und danach im Hintergrund, geschrieben wird aus einem
// eigenen Faden. Faellt die Datenbank aus, altert der Stand im Arbeitsspeicher -- der
// Dienst antwortet weiter.
package ablage

import (
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tstams/kirla-api/internal/geld"
	"github.com/tstams/kirla-api/internal/schluessel"
)

//go:embed schema.sql
var schemaSQL string

// Ablage ist der Zugang zur Datenbank.
type Ablage struct {
	teich *pgxpool.Pool
}

// Oeffne baut den Verbindungsteich auf.
func Oeffne(ctx context.Context, url string) (*Ablage, error) {
	pk, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("DATABASE_URL: %w", err)
	}
	// Klein halten: dieser Dienst spricht die Datenbank nur im Hintergrund an, und ein
	// grosser Teich verdeckt nur, dass jemand sie doch in den heissen Weg gelegt hat.
	pk.MaxConns = 4
	pk.MinConns = 1
	pk.MaxConnIdleTime = 5 * time.Minute
	// Ein haengender Verbindungsaufbau darf den Hintergrundfaden nicht ewig blockieren.
	pk.ConnConfig.ConnectTimeout = 10 * time.Second

	teich, err := pgxpool.NewWithConfig(ctx, pk)
	if err != nil {
		return nil, fmt.Errorf("verbindungsteich: %w", err)
	}
	return &Ablage{teich: teich}, nil
}

func (a *Ablage) Schliesse() { a.teich.Close() }

// Erreichbar sagt, ob die Datenbank gerade antwortet. Fuer den Abgleich und fuer die
// Bereitschaftsanzeige -- nicht fuer den Anfrageweg.
func (a *Ablage) Erreichbar(ctx context.Context) error {
	frist, abbrechen := context.WithTimeout(ctx, 5*time.Second)
	defer abbrechen()
	return a.teich.Ping(frist)
}

// Schema legt die Tabellen an, falls sie fehlen.
func (a *Ablage) Schema(ctx context.Context) error {
	_, err := a.teich.Exec(ctx, schemaSQL)
	if err != nil {
		return fmt.Errorf("schema anlegen: %w", err)
	}
	return nil
}

// SchluesselMitTopf ist ein Schluessel samt seinem Guthaben.
type SchluesselMitTopf struct {
	Eintrag  schluessel.Eintrag
	Guthaben geld.Betrag
}

// LadeSchluessel holt alle Schluessel samt Toepfen. Wird beim Start und beim Abgleich
// aufgerufen, nie im Anfrageweg.
func (a *Ablage) LadeSchluessel(ctx context.Context) ([]SchluesselMitTopf, error) {
	reihen, err := a.teich.Query(ctx, `
		SELECT s.kennung, s.hash, s.nutzer, s.gesperrt, s.angelegt,
		       COALESCE(t.guthaben_nano, 0)
		  FROM schluessel s
		  LEFT JOIN topf t ON t.kennung = s.kennung`)
	if err != nil {
		return nil, fmt.Errorf("schluessel lesen: %w", err)
	}
	defer reihen.Close()

	var aus []SchluesselMitTopf
	for reihen.Next() {
		var s SchluesselMitTopf
		var nano int64
		if err := reihen.Scan(&s.Eintrag.Kennung, &s.Eintrag.Hash, &s.Eintrag.Nutzer,
			&s.Eintrag.Gesperrt, &s.Eintrag.Angelegt, &nano); err != nil {
			return nil, fmt.Errorf("schluessel lesen: %w", err)
		}
		s.Guthaben = geld.Betrag(nano)
		aus = append(aus, s)
	}
	return aus, reihen.Err()
}

// LegeSchluesselAn traegt einen neuen Schluessel samt leerem Topf ein.
func (a *Ablage) LegeSchluesselAn(ctx context.Context, e schluessel.Eintrag, guthaben geld.Betrag) error {
	return pgx.BeginFunc(ctx, a.teich, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO schluessel (kennung, hash, nutzer, gesperrt, angelegt)
			VALUES ($1,$2,$3,$4,$5)`,
			e.Kennung, e.Hash, e.Nutzer, e.Gesperrt, e.Angelegt); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO topf (kennung, guthaben_nano) VALUES ($1,$2)`,
			e.Kennung, int64(guthaben))
		return err
	})
}

// Aufladen legt Guthaben in einen Topf. Bis Stripe kommt (Schritt 5) ist das der Weg von
// Hand.
func (a *Ablage) Aufladen(ctx context.Context, kennung string, betrag geld.Betrag) (geld.Betrag, error) {
	var neu int64
	err := a.teich.QueryRow(ctx, `
		UPDATE topf SET guthaben_nano = guthaben_nano + $2, aktualisiert = now()
		 WHERE kennung = $1
		RETURNING guthaben_nano`, kennung, int64(betrag)).Scan(&neu)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("kein topf zu %q", kennung)
	}
	return geld.Betrag(neu), err
}

// Sperre setzt oder loest die Sperre eines Schluessels.
func (a *Ablage) Sperre(ctx context.Context, kennung string, gesperrt bool) error {
	marke, err := a.teich.Exec(ctx, `UPDATE schluessel SET gesperrt = $2 WHERE kennung = $1`,
		kennung, gesperrt)
	if err != nil {
		return err
	}
	if marke.RowsAffected() == 0 {
		return fmt.Errorf("kein schluessel %q", kennung)
	}
	return nil
}

// Aufruf sind die Kopfdaten eines Aufrufs -- das, was DAUERHAFT bleiben darf (Auftrag §5).
type Aufruf struct {
	AnfrageID    string
	Kennung      string
	Nutzer       string
	Modell       string
	Zeit         time.Time
	Dauer        time.Duration
	Status       int
	Stroemend    bool
	TokensEin    int
	TokensAus    int
	Einkauf      geld.Betrag
	Verkauf      geld.Betrag
	KostenQuelle string // gemeldet | frei | boden
}

// SchreibeAufrufe traegt einen Stapel Kopfdaten ein und zieht denselben Betrag von den
// Toepfen ab -- in EINER Transaktion.
//
// Beides zusammen oder gar nicht: stuende der Aufruf ohne Abbuchung da, waere er gratis;
// stuende die Abbuchung ohne Aufruf da, koennte sie niemand erklaeren.
func (a *Ablage) SchreibeAufrufe(ctx context.Context, stapel []Aufruf) error {
	if len(stapel) == 0 {
		return nil
	}
	return pgx.BeginFunc(ctx, a.teich, func(tx pgx.Tx) error {
		stapelweise := &pgx.Batch{}
		abzug := map[string]int64{}
		for _, r := range stapel {
			stapelweise.Queue(`
				INSERT INTO aufruf (anfrage_id, kennung, nutzer, modell, zeit, dauer_ms,
				                    status, stroemend, tokens_ein, tokens_aus,
				                    einkauf_nano, verkauf_nano, kosten_quelle)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
				ON CONFLICT (anfrage_id) DO NOTHING`,
				r.AnfrageID, r.Kennung, r.Nutzer, nullWennLeer(r.Modell), r.Zeit,
				r.Dauer.Milliseconds(), r.Status, r.Stroemend, r.TokensEin, r.TokensAus,
				int64(r.Einkauf), int64(r.Verkauf), r.KostenQuelle)
			abzug[r.Kennung] += int64(r.Verkauf)
		}
		for kennung, nano := range abzug {
			if nano == 0 {
				continue
			}
			stapelweise.Queue(`
				UPDATE topf SET guthaben_nano = guthaben_nano - $2, aktualisiert = now()
				 WHERE kennung = $1`, kennung, nano)
		}
		return tx.SendBatch(ctx, stapelweise).Close()
	})
}

func nullWennLeer(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ── DIE INHALTE (Auftrag §5, Schritt 4) ────────────────────────────────────────────────

// Inhalt ist der Mitschnitt einer Anfrage samt Antwort. Er verfaellt nach 30 Tagen; die
// Kopfdaten daneben bleiben dauerhaft.
type Inhalt struct {
	AnfrageID string
	Zeit      time.Time
	// Beide Felder sind BEREITS gzip-komprimiert, und zwar ueber Klartext.
	Anfrage  []byte
	Antwort  []byte
	KamAls   string // Content-Encoding, mit dem die Antwort ankam (nur Diagnose)
	Gekuerzt bool
}

// SchreibeInhalte legt einen Stapel Mitschnitte ab.
//
// GETRENNT VON DEN KOPFDATEN, und zwar auch in der Transaktion: geht das Ablegen schief,
// darf die Abrechnung trotzdem stehen. Ein Mitschnitt ist Diagnose, eine Buchung ist Geld.
func (a *Ablage) SchreibeInhalte(ctx context.Context, stapel []Inhalt) error {
	if len(stapel) == 0 {
		return nil
	}
	stapelweise := &pgx.Batch{}
	for _, i := range stapel {
		stapelweise.Queue(`
			INSERT INTO inhalt (anfrage_id, zeit, anfrage, antwort, kam_als, gekuerzt)
			VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (anfrage_id) DO NOTHING`,
			i.AnfrageID, i.Zeit, i.Anfrage, i.Antwort, nullWennLeer(i.KamAls), i.Gekuerzt)
	}
	return a.teich.SendBatch(ctx, stapelweise).Close()
}

// LiesInhalt holt einen Mitschnitt zurueck -- entpackt, so wie er auf der Leitung stand.
func (a *Ablage) LiesInhalt(ctx context.Context, anfrageID string) (anfrage, antwort []byte, err error) {
	var roh1, roh2 []byte
	err = a.teich.QueryRow(ctx,
		`SELECT anfrage, antwort FROM inhalt WHERE anfrage_id = $1`, anfrageID).Scan(&roh1, &roh2)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("zu %q liegt kein Mitschnitt (mehr) vor", anfrageID)
	}
	if err != nil {
		return nil, nil, err
	}
	if anfrage, err = entpacke(roh1); err != nil {
		return nil, nil, err
	}
	antwort, err = entpacke(roh2)
	return anfrage, antwort, err
}

func entpacke(roh []byte) ([]byte, error) {
	if len(roh) == 0 {
		return nil, nil
	}
	aus, err := gzip.NewReader(bytes.NewReader(roh))
	if err != nil {
		return nil, err
	}
	defer aus.Close()
	return io.ReadAll(io.LimitReader(aus, 64<<20))
}

// Verfall loescht Mitschnitte, die aelter sind als `alter`, und gibt zurueck, wie viele.
//
// DIE ZAHL IST DER PUNKT. Der Auftrag §5: "Der Verfall muss laufen, nicht nur eingerichtet
// sein. Ein Aufraeumer, den niemand prueft, ist in einem halben Jahr ein voller
// Datentraeger. Schreiben Sie die Zahl der geloeschten Saetze ins Protokoll."
func (a *Ablage) Verfall(ctx context.Context, alter time.Duration) (int64, error) {
	marke, err := a.teich.Exec(ctx,
		`DELETE FROM inhalt WHERE zeit < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int64(alter.Seconds())))
	if err != nil {
		return 0, fmt.Errorf("verfall: %w", err)
	}
	return marke.RowsAffected(), nil
}

// Ablagestand sind die Zahlen, die GET /v1/route ausweist.
type Ablagestand struct {
	Saetze    int64
	Bytes     int64
	Aeltester time.Time
}

// Stand misst, was gerade abgelegt ist. Nicht im Anfrageweg -- der Wert wird im
// Hintergrund aufgefrischt.
func (a *Ablage) Stand(ctx context.Context) (Ablagestand, error) {
	var s Ablagestand
	var aeltester *time.Time
	err := a.teich.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(pg_column_size(anfrage) + pg_column_size(antwort)),0), min(zeit)
		  FROM inhalt`).Scan(&s.Saetze, &s.Bytes, &aeltester)
	if aeltester != nil {
		s.Aeltester = *aeltester
	}
	return s, err
}
