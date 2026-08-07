// Paket kulanz haelt fest, wie lange die Datenbank schon weg ist -- und was daraus folgt.
//
// DIE ENTSCHEIDUNG DAHINTER (klabs ADR-0016, Auftrag §0): die Kulanzfrist liegt IN kirla.ai
// und schuetzt nicht gegen den Ausfall des Vorbaus, sondern gegen den Ausfall von allem
// dahinter -- Autorisierungsdatenbank, Abrechnung, Stripe.
//
// Der erste Entwurf hatte sie auf der Kundenseite. Das traegt nicht: bei einem reinen
// Durchreichen gibt es ohne kirla.ai gar keinen Modellaufruf, der Kunde besitzt ja keinen
// OpenRouter-Schluessel. Eine Frist in klabs haette eine Erlaubnis verlaengert, die niemand
// einloesen kann.
//
// Die Frist ist ein ZEITFENSTER, und der Betreiber zahlt dafuer bewusst: wer kirla.ai 24
// Stunden blockiert, arbeitet 24 Stunden ohne Autorisierung.
package kulanz

import (
	"sync"
	"time"
)

// Zustand ist, woran der Vorbau sein Verhalten ausrichtet.
type Zustand int

const (
	// Gesund: die Datenbank antwortet. Es wird zurueckgelegt und bei leerem Topf mit 402
	// abgelehnt.
	Gesund Zustand = iota
	// ImFenster: die Datenbank ist weg, aber noch keine 24 Stunden. Es wird NICHT
	// zurueckgelegt (Auftrag §4) -- die Toepfe im Speicher sind veraltet, und auf einen
	// veralteten Stand hin einen 402 zu schicken, waere schlimmer als weiterzureichen.
	// Gebucht wird in eine Datei und nach der Wiederkehr nachgetragen.
	ImFenster
	// Abgelaufen: laenger als 24 Stunden ohne Datenbank. Jetzt endet die Kulanz.
	Abgelaufen
)

func (z Zustand) String() string {
	switch z {
	case Gesund:
		return "gesund"
	case ImFenster:
		return "im_fenster"
	default:
		return "abgelaufen"
	}
}

// VorgabeFenster sind die 24 Stunden aus ADR-0016.
const VorgabeFenster = 24 * time.Hour

// Lage merkt sich, wann die Datenbank zuletzt geantwortet hat.
type Lage struct {
	mu             sync.RWMutex
	letzterErfolg  time.Time
	letzterVersuch bool // ist der letzte Versuch gelungen?
	fenster        time.Duration
	jetzt          func() time.Time
}

func Neu(fenster time.Duration) *Lage {
	if fenster <= 0 {
		fenster = VorgabeFenster
	}
	return &Lage{fenster: fenster, jetzt: time.Now}
}

// Gelungen meldet einen erfolgreichen Zugriff auf die Datenbank.
func (l *Lage) Gelungen() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.letzterErfolg = l.jetzt()
	l.letzterVersuch = true
}

// Gescheitert meldet einen misslungenen Zugriff.
func (l *Lage) Gescheitert() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.letzterVersuch = false
}

// Zustand sagt, woran der Vorbau ist, und wie alt der letzte bekannte Stand ist.
func (l *Lage) Zustand() (Zustand, time.Duration) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if l.letzterErfolg.IsZero() {
		// Es hat noch NIE geklappt. Das ist kein Kulanzfall, sondern ein Dienst, der
		// nie richtig angelaufen ist -- er kennt weder Schluessel noch Toepfe.
		return Abgelaufen, 0
	}
	alter := l.jetzt().Sub(l.letzterErfolg)
	switch {
	case l.letzterVersuch:
		return Gesund, alter
	case alter < l.fenster:
		return ImFenster, alter
	default:
		return Abgelaufen, alter
	}
}

// Rest sagt, wie lange das Fenster noch traegt. Null, wenn es zu ist oder gar nicht laeuft.
func (l *Lage) Rest() time.Duration {
	zustand, alter := l.Zustand()
	if zustand != ImFenster {
		return 0
	}
	return l.fenster - alter
}
