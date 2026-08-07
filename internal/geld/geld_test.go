package geld

import (
	"strings"
	"testing"
)

// DIE ZAHL, DIE DIESES PAKET AUSGELOEST HAT. OpenRouter meldete am 07.08.2026 einen
// Verbrauch von 0.016653351 -- neun Nachkommastellen. In Mikro-Dollar waere die letzte
// Stelle weg, systematisch und immer in dieselbe Richtung.
func TestNeunStellenUeberlebenDenUmweg(t *testing.T) {
	b, err := AusText("0.016653351")
	if err != nil {
		t.Fatal(err)
	}
	if b != 16_653_351 {
		t.Errorf("%d Nano statt 16653351 — eine Stelle ging verloren", int64(b))
	}
	if got := b.Text(9); got != "0.016653351" {
		t.Errorf("zurueck als %q", got)
	}
}

// KEIN UMWEG UEBER GLEITKOMMA. Der Test faellt, sobald jemand AusText auf ParseFloat
// umstellt: 0,1 + 0,2 ist dort nicht 0,3.
func TestAddierenDriftetNicht(t *testing.T) {
	a, _ := AusText("0.1")
	b, _ := AusText("0.2")
	if a+b != AusDollar(0.3) {
		t.Errorf("0,1 + 0,2 = %s", (a + b).Text(9))
	}

	// Zehntausend Abbuchungen, wie sie der Auftrag §5 fuer einen Tag nennt.
	topf, _ := AusText("100")
	einzeln, _ := AusText("0.001234567")
	for i := 0; i < 10_000; i++ {
		topf -= einzeln
	}
	erwartet, _ := AusText("87.65433")
	if topf != erwartet {
		t.Errorf("nach 10 000 Abbuchungen %s statt %s", topf.Text(9), erwartet.Text(9))
	}
}

func TestAusText(t *testing.T) {
	for _, f := range []struct {
		ein  string
		nano int64
	}{
		{"0", 0},
		{"1", 1_000_000_000},
		{"0.002", 2_000_000},
		{"-0.5", -500_000_000},
		{".25", 250_000_000},
		{"  1.5  ", 1_500_000_000},
		{"1e-7", 100},
		{"2.5E-3", 2_500_000},
		// Mehr als Nano wird ABGESCHNITTEN, nicht gerundet: aufrunden hiesse, einen
		// Bruchteil zu berechnen, den niemand gemeldet hat.
		{"0.0000000009", 0},
		{"0.1234567891", 123_456_789},
	} {
		t.Run(f.ein, func(t *testing.T) {
			b, err := AusText(f.ein)
			if err != nil {
				t.Fatal(err)
			}
			if int64(b) != f.nano {
				t.Errorf("%q -> %d, erwartet %d", f.ein, int64(b), f.nano)
			}
		})
	}
}

func TestUnlesbaresWirdNichtStillNull(t *testing.T) {
	// Eine kaputte Angabe darf NICHT als 0 durchgehen -- sonst ist ein Modell mit
	// unlesbarem Preis gratis, und genau so entsteht eine Rechnung, die niemand erwartet.
	for _, s := range []string{"", "   ", "kostenlos", "0.0.1", "$1", "abc"} {
		if _, err := AusText(s); err == nil {
			t.Errorf("%q ging als Betrag durch", s)
		}
	}
}

// DER AUFSCHLAG MUSS AUSGEWIESEN SEIN (Auftrag §4) -- und zwar genau in der Form, die
// dort steht.
func TestDieRechnungStehtSoDaWieImAuftrag(t *testing.T) {
	einkauf, _ := AusText("0.0042")
	got := Erklaerung(einkauf, 1500)
	if !strings.Contains(got, "0.004200") || !strings.Contains(got, "15 %") || !strings.Contains(got, "0.004830") {
		t.Errorf("die Rechnung liest sich nicht nach: %q", got)
	}
}

func TestAufschlag(t *testing.T) {
	einkauf, _ := AusText("0.0042")
	mit := einkauf.MitAufschlag(1500)
	if mit != 4_830_000 {
		t.Errorf("0,0042 + 15 %% = %s statt 0.004830", mit.Text(6))
	}
	if einkauf.Aufschlag(1500) != mit-einkauf {
		t.Error("Aufschlag und Differenz stimmen nicht ueberein")
	}
	// Ohne Aufschlag bleibt der Betrag, wie er ist.
	if einkauf.MitAufschlag(0) != einkauf {
		t.Error("0 %% hat den Betrag veraendert")
	}
}

// EIN AUFSCHLAG AUF NULL BLEIBT NULL. Sonst kostet ein kostenloses Modell ploetzlich etwas.
func TestAufschlagAufNullBleibtNull(t *testing.T) {
	if Betrag(0).MitAufschlag(1500) != 0 {
		t.Error("aus 0 wurde mit Aufschlag etwas")
	}
}

func TestText(t *testing.T) {
	b, _ := AusText("1.234567891")
	for _, f := range []struct {
		stellen  int
		erwartet string
	}{
		{0, "1"},
		{2, "1.23"},
		{6, "1.234567"},
		{9, "1.234567891"},
	} {
		if got := b.Text(f.stellen); got != f.erwartet {
			t.Errorf("Text(%d) = %q, erwartet %q", f.stellen, got, f.erwartet)
		}
	}
	n, _ := AusText("-0.5")
	if got := n.Text(2); got != "-0.50" {
		t.Errorf("negativ: %q", got)
	}
}
