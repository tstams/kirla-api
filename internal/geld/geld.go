// Paket geld rechnet in Nano-Dollar statt in Gleitkommazahlen.
//
// WARUM NICHT float64: Geld in Gleitkomma sammelt Fehler. 0,1 + 0,2 ist dort nicht 0,3, und
// ein Topf, aus dem zehntausendmal am Tag abgebucht wird, driftet. Die Differenz ist winzig
// und der Streit darueber teuer -- ein Kunde, dessen Guthaben nicht aufgeht, hat recht, und
// man kann ihm nicht zeigen, wo es geblieben ist.
//
// WARUM NANO UND NICHT MIKRO: OpenRouter meldet Kosten mit neun Nachkommastellen, gemessen
// am 07.08.2026: usage = 0.016653351. In Mikro-Dollar (sechs Stellen) faellt der Rest weg.
// Einzeln ist das nichts; bei 10 000 Aufrufen am Tag (Auftrag §5) ist es ein systematischer
// Fehler, und er geht immer in dieselbe Richtung. int64 in Nano reicht bis 9,2 Milliarden
// Dollar -- die Grenze wird dieses Produkt nicht erleben.
package geld

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Betrag ist ein Geldbetrag in Nano-Dollar (1 USD = 1e9).
type Betrag int64

// Nano je Dollar.
const Nano = 1_000_000_000

// ErrUnlesbar heisst: da stand etwas, aber kein Betrag.
var ErrUnlesbar = errors.New("kein lesbarer Betrag")

// AusDollar wandelt einen Dollarwert um. Nur fuer Konstanten im Quelltext und fuer Werte,
// die schon als Zahl vorliegen -- fuer Text nimm AusText, das nicht ueber float64 geht.
func AusDollar(d float64) Betrag {
	return Betrag(math.Round(d * Nano))
}

// AusText liest einen Dezimalbetrag OHNE Umweg ueber Gleitkomma.
//
// Der Umweg waere der Fehler, den dieses Paket vermeiden soll: strconv.ParseFloat("0.016653351")
// liefert eine Zahl, die schon nicht mehr genau der Text ist, und die Rundung danach
// zementiert es.
func AusText(s string) (Betrag, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrUnlesbar
	}
	minus := false
	switch s[0] {
	case '-':
		minus, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	// Wissenschaftliche Schreibweise kommt bei sehr kleinen Kosten vor (1e-7).
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		return ausWissenschaft(s, i, minus)
	}

	ganz, bruch, _ := strings.Cut(s, ".")
	if ganz == "" {
		ganz = "0"
	}
	if len(bruch) > 9 {
		// Mehr als Nano-Genauigkeit wird abgeschnitten und nicht gerundet: aufrunden
		// hiesse, dem Kunden einen Bruchteil zu berechnen, den niemand gemeldet hat.
		bruch = bruch[:9]
	}
	for len(bruch) < 9 {
		bruch += "0"
	}
	g, err := strconv.ParseInt(ganz, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrUnlesbar, s)
	}
	b, err := strconv.ParseInt(bruch, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrUnlesbar, s)
	}
	wert := Betrag(g*Nano + b)
	if minus {
		wert = -wert
	}
	return wert, nil
}

func ausWissenschaft(s string, i int, minus bool) (Betrag, error) {
	// Hier ist der Umweg ueber float64 vertretbar: die Schreibweise kommt nur bei
	// verschwindend kleinen Betraegen vor, wo die letzte Nano-Stelle ohnehin null ist.
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrUnlesbar, s)
	}
	wert := AusDollar(f)
	if minus {
		wert = -wert
	}
	return wert, nil
}

// Dollar gibt den Betrag als Gleitkommazahl -- NUR fuer die Anzeige und fuer JSON, nie
// fuer eine Rechnung.
func (b Betrag) Dollar() float64 { return float64(b) / Nano }

// String zeigt den Betrag mit sechs Stellen: genug, um Zehntel-Cent zu sehen, wenig genug,
// dass eine Rechnung lesbar bleibt.
func (b Betrag) String() string { return b.Text(6) }

// Text zeigt den Betrag mit n Nachkommastellen, ohne Gleitkomma.
func (b Betrag) Text(stellen int) string {
	if stellen < 0 {
		stellen = 0
	}
	if stellen > 9 {
		stellen = 9
	}
	minus := b < 0
	if minus {
		b = -b
	}
	ganz := int64(b) / Nano
	bruch := int64(b) % Nano
	aus := strconv.FormatInt(ganz, 10)
	if stellen > 0 {
		s := fmt.Sprintf("%09d", bruch)[:stellen]
		aus += "." + s
	}
	if minus {
		aus = "-" + aus
	}
	return aus
}

// MitAufschlag legt den Aufschlag auf und rundet kaufmaennisch.
//
// Der Aufschlag steht in Hundertstel Prozent (1500 = 15,00 %), damit auch er keine
// Gleitkommazahl ist.
func (b Betrag) MitAufschlag(hundertstelProzent int64) Betrag {
	// b * (10000 + p) / 10000, mit Rundung auf das naechste Nano.
	zaehler := int64(b) * (10_000 + hundertstelProzent)
	if zaehler >= 0 {
		return Betrag((zaehler + 5_000) / 10_000)
	}
	return Betrag((zaehler - 5_000) / 10_000)
}

// Aufschlag ist der Anteil, der auf den Einkauf kommt.
func (b Betrag) Aufschlag(hundertstelProzent int64) Betrag {
	return b.MitAufschlag(hundertstelProzent) - b
}

// Erklaerung schreibt die Rechnung so hin, wie sie ein Mensch nachvollziehen kann.
//
// Der Auftrag §4 verlangt das ausdruecklich: "Der Aufschlag muss ausgewiesen sein. Ein
// Aufschlag, den man nicht sieht, ist keine Abrechnung, sondern eine Ueberraschung."
func Erklaerung(einkauf Betrag, hundertstelProzent int64) string {
	ganz := hundertstelProzent / 100
	rest := hundertstelProzent % 100
	prozent := strconv.FormatInt(ganz, 10)
	if rest != 0 {
		prozent += "," + fmt.Sprintf("%02d", rest)
	}
	return fmt.Sprintf("OpenRouter %s $ + %s %% = %s $",
		einkauf.Text(6), prozent, einkauf.MitAufschlag(hundertstelProzent).Text(6))
}
