package ablage

import (
	"context"
	"log/slog"
	"time"

	"github.com/tstams/kirla-api/internal/geld"
	"github.com/tstams/kirla-api/internal/kulanz"
	"github.com/tstams/kirla-api/internal/topf"
)

// Schreiber leert den Kopfdaten-Kanal in die Datenbank -- in einem EIGENEN Faden.
//
// Das ist die Stelle, an der die Betriebsregel aus §0 durchgesetzt wird: der Anfrageweg
// legt seine Kopfdaten in einen gepufferten Kanal und ist fertig. Ob die Datenbank
// antwortet, erfaehrt er nie.
type Schreiber struct {
	Ablage    *Ablage
	Toepfe    *topf.Toepfe
	Eingang   <-chan Aufruf
	Protokoll *slog.Logger
	Lage      *kulanz.Lage

	// Notfall faengt auf, was nicht in die Datenbank kann (Auftrag §4: "nur
	// mitgeschrieben — in eine Datei, die nach der Wiederkehr abgearbeitet wird").
	// Fehlt er, geht die Abrechnung verloren und es steht laut im Protokoll.
	Notfall func([]Aufruf) error

	// Stapel ist, wie viele Saetze hoechstens auf einmal geschrieben werden.
	Stapel int
	// Takt ist, wie lange gewartet wird, bis ein angefangener Stapel auch unvollstaendig
	// geschrieben wird.
	Takt time.Duration
}

// Laufe blockiert, bis der Kanal geschlossen wird oder der Kontext endet.
func (s *Schreiber) Laufe(ctx context.Context) {
	if s.Stapel <= 0 {
		s.Stapel = 64
	}
	if s.Takt <= 0 {
		s.Takt = 2 * time.Second
	}
	uhr := time.NewTicker(s.Takt)
	defer uhr.Stop()

	stapel := make([]Aufruf, 0, s.Stapel)
	schreibe := func() {
		if len(stapel) == 0 {
			return
		}
		s.schreibe(ctx, stapel)
		stapel = stapel[:0]
	}

	for {
		select {
		case a, offen := <-s.Eingang:
			if !offen {
				schreibe()
				return
			}
			stapel = append(stapel, a)
			if len(stapel) >= s.Stapel {
				schreibe()
			}
		case <-uhr.C:
			schreibe()
		case <-ctx.Done():
			// Was noch da ist, wird versucht -- danach ist Schluss.
			schreibe()
			return
		}
	}
}

func (s *Schreiber) schreibe(ctx context.Context, stapel []Aufruf) {
	// Eigene Frist: der Schreiber darf beim Beenden nicht ewig haengen, aber auch nicht
	// sofort aufgeben, wenn die Datenbank gerade langsam ist.
	frist, abbrechen := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer abbrechen()

	if err := s.Ablage.SchreibeAufrufe(frist, stapel); err != nil {
		if s.Lage != nil {
			s.Lage.Gescheitert()
		}
		var summe geld.Betrag
		for _, a := range stapel {
			summe += a.Verkauf
		}
		// DIE DATEI IST DIE EINZIGE SPUR, solange die Datenbank weg ist.
		if s.Notfall != nil {
			if notErr := s.Notfall(stapel); notErr == nil {
				s.Protokoll.Warn("datenbank weg — kopfdaten ins nachbuch",
					"saetze", len(stapel), "summe_usd", summe.Text(6), "fehler", err)
				return
			} else {
				s.Protokoll.Error("nachbuch nicht schreibbar", "fehler", notErr)
			}
		}
		// Weder Datenbank noch Nachbuch: JETZT ist Abrechnung wirklich weg.
		s.Protokoll.Error("kopfdaten nicht geschrieben — ABRECHNUNG VERLOREN",
			"saetze", len(stapel), "summe_usd", summe.Text(6), "fehler", err)
		return
	}
	if s.Lage != nil {
		s.Lage.Gelungen()
	}

	// Erst NACH dem erfolgreichen Schreiben darf der Verbrauch aus den Toepfen im
	// Arbeitsspeicher verschwinden -- sonst zieht der naechste Abgleich denselben Betrag
	// ein zweites Mal ab.
	if s.Toepfe != nil {
		for _, a := range stapel {
			s.Toepfe.Abgeglichen(a.Kennung, a.Verkauf)
		}
	}
	s.Protokoll.Debug("kopfdaten geschrieben", "saetze", len(stapel))
}

// Abgleicher holt Schluessel und Toepfe regelmaessig aus der Datenbank in den
// Arbeitsspeicher.
//
// Faellt die Datenbank aus, schlaegt das hier fehl und der Stand im Arbeitsspeicher
// ALTERT einfach -- der Vorbau antwortet weiter. Genau das ist die Kulanzfrist aus §0;
// Schritt 3 zieht die 24-Stunden-Grenze ein.
type Abgleicher struct {
	Ablage    *Ablage
	Toepfe    *topf.Toepfe
	Protokoll *slog.Logger
	Takt      time.Duration
	Lage      *kulanz.Lage
	// Uebernimm bekommt die frisch geladenen Schluessel.
	Uebernimm func([]SchluesselMitTopf)
	// NachDerWiederkehr laeuft, wenn ein Abgleich nach einem Ausfall wieder gelingt --
	// dort wird das Nachbuch abgearbeitet.
	NachDerWiederkehr func(context.Context)

	// Zaehler fuer die Daempfung der Protokollzeilen waehrend eines Ausfalls. Nur aus
	// Einmal() heraus benutzt, und das laeuft in genau einem Faden.
	stille             int
	abgelaufenGemeldet bool
}

func (a *Abgleicher) Laufe(ctx context.Context) {
	if a.Takt <= 0 {
		a.Takt = 30 * time.Second
	}
	uhr := time.NewTicker(a.Takt)
	defer uhr.Stop()
	for {
		select {
		case <-uhr.C:
			a.Einmal(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// Einmal gleicht einmal ab. Fehler werden protokolliert und nicht weitergereicht: ein
// misslungener Abgleich ist kein Grund, den Dienst anzuhalten.
func (a *Abgleicher) Einmal(ctx context.Context) {
	frist, abbrechen := context.WithTimeout(ctx, 15*time.Second)
	defer abbrechen()

	// ── ERST NACHBUCHEN, DANN DEN STAND HOLEN ────────────────────────────────────────
	//
	// Das Nachbuch traegt seine Abbuchungen in die Datenbank ein. Wer zuerst den Stand
	// holt und danach nachbucht, hat im Speicher genau den Stand VON VORHER -- die Toepfe
	// waeren fuer einen Takt zu grosszuegig. Andersherum stimmt es sofort.
	//
	// Ist die Datenbank noch weg, scheitert das Nachbuchen still und die Datei bleibt
	// liegen. Genau dafuer ist sie da.
	if a.Lage != nil && a.NachDerWiederkehr != nil {
		if z, _ := a.Lage.Zustand(); z != kulanz.Gesund {
			a.NachDerWiederkehr(ctx)
		}
	}

	liste, err := a.Ablage.LadeSchluessel(frist)
	if err != nil {
		if a.Lage == nil {
			a.Protokoll.Error("abgleich gescheitert — der stand im speicher altert weiter", "fehler", err)
			return
		}
		a.Lage.Gescheitert()
		z, alter := a.Lage.Zustand()

		// ── NICHT BEI JEDEM TAKT SCHREIEN ────────────────────────────────────────────
		//
		// Der Abgleich laeuft alle 30 Sekunden. Ein Ausfall ueber das volle Fenster
		// ergaebe 2880 ERROR-Zeilen, jede mit dem vollstaendigen Verbindungsfehler --
		// auf einem Host, dessen kirla-ops-alert-Units auf Fehler schauen, deckt das
		// jeden anderen Befund zu. Der erste Ausfall ist laut, danach alle fuenf
		// Minuten eine Zeile, und beim Uebergang ins Abgelaufene wieder laut.
		a.stille++
		laut := a.stille == 1 || a.stille%10 == 0 || (z == kulanz.Abgelaufen && !a.abgelaufenGemeldet)
		if z == kulanz.Abgelaufen {
			a.abgelaufenGemeldet = true
		}
		if laut {
			stufe := slog.LevelWarn
			if z == kulanz.Abgelaufen {
				stufe = slog.LevelError
			}
			a.Protokoll.Log(ctx, stufe, "abgleich gescheitert — der stand im speicher altert weiter",
				"zustand", z.String(), "alter_minuten", int(alter.Minutes()),
				"rest_stunden", a.Lage.Rest().Hours(), "versuche", a.stille, "fehler", err)
		}
		return
	}
	if a.stille > 0 {
		a.Protokoll.Info("datenbank wieder da", "gescheiterte_versuche", a.stille)
		a.stille = 0
		a.abgelaufenGemeldet = false
	}
	for _, s := range liste {
		a.Toepfe.Setze(s.Eintrag.Kennung, s.Guthaben)
	}
	if a.Uebernimm != nil {
		a.Uebernimm(liste)
	}
	if a.Lage != nil {
		a.Lage.Gelungen()
	}
}
