// Paket ablage ist die Datenbankseite: Schluessel, Toepfe, Kopfdaten.
//
// KEINE FUNKTION AUS DIESEM PAKET WIRD IM ANFRAGEWEG AUFGERUFEN. Das ist die Betriebsregel
// des Auftrags (§0) und der Grund, warum der Vorbau eine Datenbank ueberhaupt vertragen
// kann: gelesen wird beim Start und danach im Hintergrund, geschrieben wird aus einem
// eigenen Faden. Faellt die Datenbank aus, altert der Stand im Arbeitsspeicher -- der
// Dienst antwortet weiter.
package ablage

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
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
