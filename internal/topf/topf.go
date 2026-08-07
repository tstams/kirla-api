// Paket topf haelt die Guthabentoepfe je Schluessel.
//
// ALLES HIER STEHT IM ARBEITSSPEICHER, und das ist die zentrale Entscheidung des Auftrags
// (§0): der heisse Weg darf nichts anfassen muessen. Kein Datenbankzugriff im Anfrageweg.
// Die Datenbank fuellt diese Toepfe beim Start und gleicht sie im Hintergrund ab; faellt
// sie aus, altert der Stand hier einfach weiter, statt dass der Dienst stehenbleibt.
//
// Die Abrechnung ist zweistufig, weil die echten Kosten erst NACH der Antwort feststehen
// (Auftrag §4):
//
//  1. ZURUECKLEGEN vor dem Weiterreichen -- eine Schaetzung. Reicht das Guthaben nicht,
//     sofort 402, BEVOR bei OpenRouter Kosten entstehen.
//  2. ABRECHNEN nach der Antwort mit den gemeldeten Kosten. Die Differenz geht zurueck.
//
// Dieselbe Mechanik wie `budget.Ledger` im klabs-Kern, und aus demselben Grund.
package topf

import (
	"errors"
	"sync"
	"time"

	"github.com/tstams/kirla-api/internal/geld"
)

// ErrLeer heisst: der Topf traegt diese Anfrage nicht mehr. Fuehrt zu 402.
var ErrLeer = errors.New("guthaben aufgebraucht")

// ErrUnbekannt heisst: zu diesem Schluessel gibt es keinen Topf.
var ErrUnbekannt = errors.New("kein topf zu diesem schluessel")

// Stand ist der Zustand eines Topfes, wie ihn ein Mensch sehen will.
type Stand struct {
	Kennung       string
	Guthaben      geld.Betrag // was zuletzt aus der Datenbank kam
	Verbraucht    geld.Betrag // seither abgerechnet, noch nicht abgeglichen
	Zurueckgelegt geld.Betrag // fuer gerade laufende Anfragen reserviert
}

// Verfuegbar ist, was eine neue Anfrage noch tragen kann.
func (s Stand) Verfuegbar() geld.Betrag {
	return s.Guthaben - s.Verbraucht - s.Zurueckgelegt
}

// Rest ist, was nach den laufenden Anfragen uebrig bleibt -- die Zahl, die der Kunde in
// X-Kirla-Guthaben-USD sehen soll.
func (s Stand) Rest() geld.Betrag { return s.Guthaben - s.Verbraucht }

type topf struct {
	guthaben      geld.Betrag
	verbraucht    geld.Betrag
	zurueckgelegt geld.Betrag
}

// Quittung ist der Beleg ueber eine Ruecklage. Sie MUSS abgerechnet werden, sonst bleibt
// der Betrag fuer immer reserviert und der Topf schrumpft, ohne dass jemand etwas gekauft
// hat.
type Quittung struct {
	Kennung     string
	Schaetzung  geld.Betrag
	Zeit        time.Time
	abgerechnet bool
}

// Toepfe ist die Sammlung.
type Toepfe struct {
	mu   sync.Mutex
	nach map[string]*topf
	// jetzt ist auswechselbar, damit die Tests nicht warten muessen.
	jetzt func() time.Time
}

func Neu() *Toepfe {
	return &Toepfe{nach: map[string]*topf{}, jetzt: time.Now}
}

// Setze traegt den Stand aus der Datenbank ein. Ein laufender Verbrauch bleibt erhalten:
// er ist noch nicht in der Zahl enthalten, die von dort kam.
func (t *Toepfe) Setze(kennung string, guthaben geld.Betrag) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if vorhanden, da := t.nach[kennung]; da {
		vorhanden.guthaben = guthaben
		return
	}
	t.nach[kennung] = &topf{guthaben: guthaben}
}

// Abgeglichen meldet, dass ein Verbrauch dauerhaft in der Datenbank steht und nicht mehr
// zusaetzlich vom Guthaben abgezogen werden muss.
//
// OHNE DIESEN SCHRITT WIRD DOPPELT ABGEZOGEN: der Hintergrundschreiber traegt den
// Verbrauch in die Datenbank, der naechste Abgleich holt das kleinere Guthaben zurueck --
// und `verbraucht` steht immer noch daneben.
func (t *Toepfe) Abgeglichen(kennung string, betrag geld.Betrag) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if p, da := t.nach[kennung]; da {
		p.verbraucht -= betrag
		if p.verbraucht < 0 {
			p.verbraucht = 0
		}
	}
}

// Zuruecklegen reserviert die Schaetzung. Reicht das Guthaben nicht, kommt ErrLeer und der
// Aufrufer antwortet mit 402 -- bevor bei OpenRouter Kosten entstehen.
func (t *Toepfe) Zuruecklegen(kennung string, schaetzung geld.Betrag) (*Quittung, error) {
	if schaetzung < 0 {
		schaetzung = 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	p, da := t.nach[kennung]
	if !da {
		return nil, ErrUnbekannt
	}
	if p.guthaben-p.verbraucht-p.zurueckgelegt < schaetzung {
		return nil, ErrLeer
	}
	p.zurueckgelegt += schaetzung
	return &Quittung{Kennung: kennung, Schaetzung: schaetzung, Zeit: t.jetzt()}, nil
}

// Abrechnen loest die Ruecklage auf und bucht, was wirklich angefallen ist.
//
// Zweimal dieselbe Quittung abzurechnen ist ein Fehler im Aufrufer und wuerde den Topf
// verfaelschen; die Quittung merkt es sich und der zweite Aufruf tut nichts.
func (t *Toepfe) Abrechnen(q *Quittung, tatsaechlich geld.Betrag) {
	if q == nil || q.abgerechnet {
		return
	}
	if tatsaechlich < 0 {
		tatsaechlich = 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	q.abgerechnet = true
	p, da := t.nach[q.Kennung]
	if !da {
		return
	}
	p.zurueckgelegt -= q.Schaetzung
	if p.zurueckgelegt < 0 {
		p.zurueckgelegt = 0
	}
	p.verbraucht += tatsaechlich
}

// Freigeben loest die Ruecklage auf, ohne etwas zu buchen -- fuer den Fall, dass die
// Anfrage OpenRouter nie erreicht hat und deshalb nichts gekostet haben kann.
func (t *Toepfe) Freigeben(q *Quittung) { t.Abrechnen(q, 0) }

// Stand gibt den Zustand eines Topfes.
func (t *Toepfe) Stand(kennung string) (Stand, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, da := t.nach[kennung]
	if !da {
		return Stand{}, false
	}
	return Stand{Kennung: kennung, Guthaben: p.guthaben,
		Verbraucht: p.verbraucht, Zurueckgelegt: p.zurueckgelegt}, true
}

// Alle gibt alle Staende -- fuer den Abgleich und fuer die Anzeige.
func (t *Toepfe) Alle() []Stand {
	t.mu.Lock()
	defer t.mu.Unlock()
	aus := make([]Stand, 0, len(t.nach))
	for k, p := range t.nach {
		aus = append(aus, Stand{Kennung: k, Guthaben: p.guthaben,
			Verbraucht: p.verbraucht, Zurueckgelegt: p.zurueckgelegt})
	}
	return aus
}
