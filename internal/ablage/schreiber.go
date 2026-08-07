package ablage

import (
	"context"
	"log/slog"
	"time"

	"github.com/tstams/kirla-api/internal/geld"
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
		// HIER GEHT ABRECHNUNG VERLOREN, und das muss laut im Protokoll stehen.
		// Ab Schritt 3 faengt eine Datei diese Saetze auf und arbeitet sie nach der
		// Wiederkehr ab (Auftrag §4, Kulanzfenster).
		var summe geld.Betrag
		for _, a := range stapel {
			summe += a.Verkauf
		}
		s.Protokoll.Error("kopfdaten nicht geschrieben — abrechnung fehlt",
			"saetze", len(stapel), "summe_usd", summe.Text(6), "fehler", err)
		return
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
	// Uebernimm bekommt die frisch geladenen Schluessel.
	Uebernimm func([]SchluesselMitTopf)
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

	liste, err := a.Ablage.LadeSchluessel(frist)
	if err != nil {
		a.Protokoll.Error("abgleich gescheitert — der stand im speicher altert weiter", "fehler", err)
		return
	}
	for _, s := range liste {
		a.Toepfe.Setze(s.Eintrag.Kennung, s.Guthaben)
	}
	if a.Uebernimm != nil {
		a.Uebernimm(liste)
	}
}
