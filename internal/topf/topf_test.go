package topf

import (
	"errors"
	"sync"
	"testing"

	"github.com/tstams/kirla-api/internal/geld"
)

func dollar(t *testing.T, s string) geld.Betrag {
	t.Helper()
	b, err := geld.AusText(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// DER 402 KOMMT, BEVOR KOSTEN ENTSTEHEN. Das ist der ganze Zweck des Zuruecklegens
// (Auftrag §4): reicht das Guthaben nicht, wird OpenRouter gar nicht erst gefragt.
func TestEinLeererTopfTraegtKeineAnfrage(t *testing.T) {
	tp := Neu()
	tp.Setze("k1", dollar(t, "0.005"))

	// Die erste Anfrage passt.
	q, err := tp.Zuruecklegen("k1", dollar(t, "0.004"))
	if err != nil {
		t.Fatalf("die erste Anfrage wurde abgelehnt: %v", err)
	}
	// Die zweite nicht mehr -- die erste laeuft noch und haelt ihre Ruecklage.
	if _, err := tp.Zuruecklegen("k1", dollar(t, "0.004")); !errors.Is(err, ErrLeer) {
		t.Fatalf("die zweite Anfrage kam durch: %v", err)
	}

	// Nach der Abrechnung mit den echten (kleineren) Kosten ist wieder Platz.
	tp.Abrechnen(q, dollar(t, "0.001"))
	if _, err := tp.Zuruecklegen("k1", dollar(t, "0.004")); err != nil {
		t.Fatalf("nach der Abrechnung ist der Platz nicht zurueck: %v", err)
	}
}

// DIE DIFFERENZ GEHT ZURUECK IN DEN TOPF (Auftrag §4). Eine Schaetzung, die zu hoch war,
// darf nicht als Verbrauch stehen bleiben -- sonst zahlt der Kunde fuer Tokens, die das
// Modell nie erzeugt hat.
func TestDieDifferenzGehtZurueck(t *testing.T) {
	tp := Neu()
	tp.Setze("k1", dollar(t, "1"))

	q, err := tp.Zuruecklegen("k1", dollar(t, "0.5"))
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := tp.Stand("k1"); s.Verfuegbar() != dollar(t, "0.5") {
		t.Errorf("waehrend der Anfrage verfuegbar: %s", s.Verfuegbar())
	}

	tp.Abrechnen(q, dollar(t, "0.02"))
	s, _ := tp.Stand("k1")
	if s.Verfuegbar() != dollar(t, "0.98") {
		t.Errorf("nach der Abrechnung verfuegbar %s statt 0,98", s.Verfuegbar())
	}
	if s.Zurueckgelegt != 0 {
		t.Errorf("es blieb etwas zurueckgelegt: %s", s.Zurueckgelegt)
	}
}

// EINE QUITTUNG DARF NUR EINMAL WIRKEN. Wer zweimal abrechnet, bucht zweimal ab -- und der
// Fehler faellt erst auf, wenn das Guthaben nicht mehr aufgeht.
func TestZweimalAbrechnenBuchtNichtZweimal(t *testing.T) {
	tp := Neu()
	tp.Setze("k1", dollar(t, "1"))
	q, _ := tp.Zuruecklegen("k1", dollar(t, "0.1"))

	tp.Abrechnen(q, dollar(t, "0.05"))
	tp.Abrechnen(q, dollar(t, "0.05"))

	s, _ := tp.Stand("k1")
	if s.Verbraucht != dollar(t, "0.05") {
		t.Errorf("verbraucht %s statt 0,05 — es wurde doppelt gebucht", s.Verbraucht)
	}
}

// EINE ANFRAGE, DIE OPENROUTER NIE ERREICHT HAT, KOSTET NICHTS.
func TestFreigebenBuchtNichts(t *testing.T) {
	tp := Neu()
	tp.Setze("k1", dollar(t, "1"))
	q, _ := tp.Zuruecklegen("k1", dollar(t, "0.3"))
	tp.Freigeben(q)

	s, _ := tp.Stand("k1")
	if s.Verbraucht != 0 || s.Zurueckgelegt != 0 {
		t.Errorf("verbraucht=%s zurueckgelegt=%s — es blieb etwas haengen", s.Verbraucht, s.Zurueckgelegt)
	}
	if s.Verfuegbar() != dollar(t, "1") {
		t.Errorf("verfuegbar %s statt 1", s.Verfuegbar())
	}
}

// OHNE TOPF KEINE ANFRAGE -- ein Schluessel ohne Guthabentopf darf nicht unbegrenzt
// einkaufen.
func TestOhneTopfWirdAbgelehnt(t *testing.T) {
	tp := Neu()
	if _, err := tp.Zuruecklegen("gibtsnicht", dollar(t, "0.01")); !errors.Is(err, ErrUnbekannt) {
		t.Fatalf("ein Schluessel ohne Topf kam durch: %v", err)
	}
}

// DER ABGLEICH DARF NICHT DOPPELT ABZIEHEN. Der Hintergrundschreiber traegt den Verbrauch
// in die Datenbank; kommt von dort das kleinere Guthaben zurueck, muss `verbraucht` um
// denselben Betrag sinken. Ohne das schrumpft der Topf zweimal.
func TestNachDemAbgleichWirdNichtDoppeltAbgezogen(t *testing.T) {
	tp := Neu()
	tp.Setze("k1", dollar(t, "10"))
	q, _ := tp.Zuruecklegen("k1", dollar(t, "1"))
	tp.Abrechnen(q, dollar(t, "1"))

	if s, _ := tp.Stand("k1"); s.Rest() != dollar(t, "9") {
		t.Fatalf("Rest %s statt 9", s.Rest())
	}

	// Der Schreiber hat gebucht; die Datenbank meldet jetzt 9.
	tp.Abgeglichen("k1", dollar(t, "1"))
	tp.Setze("k1", dollar(t, "9"))

	s, _ := tp.Stand("k1")
	if s.Rest() != dollar(t, "9") {
		t.Errorf("Rest %s statt 9 — es wurde doppelt abgezogen", s.Rest())
	}
	if s.Verbraucht != 0 {
		t.Errorf("verbraucht steht noch auf %s", s.Verbraucht)
	}
}

// GLEICHZEITIGE ANFRAGEN DUERFEN DEN TOPF NICHT UEBERZIEHEN. Der Auftrag §6 nennt Spitzen
// von 20-30 gleichzeitigen Stroemen, weil alle Instanzen auf der vollen Viertelstunde
// takten -- genau dann greift diese Grenze.
func TestGleichzeitigeAnfragenUeberziehenNicht(t *testing.T) {
	tp := Neu()
	tp.Setze("k1", dollar(t, "1"))
	einzeln := dollar(t, "0.1") // genau zehn passen

	var wg sync.WaitGroup
	var mu sync.Mutex
	var durch int
	quittungen := []*Quittung{}

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q, err := tp.Zuruecklegen("k1", einzeln)
			if err != nil {
				return
			}
			mu.Lock()
			durch++
			quittungen = append(quittungen, q)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if durch != 10 {
		t.Errorf("%d Anfragen kamen durch, es passen genau 10", durch)
	}
	if s, _ := tp.Stand("k1"); s.Verfuegbar() != 0 {
		t.Errorf("verfuegbar %s statt 0", s.Verfuegbar())
	}
	// Und nach der Abrechnung ist alles wieder sauber.
	for _, q := range quittungen {
		tp.Abrechnen(q, 0)
	}
	if s, _ := tp.Stand("k1"); s.Verfuegbar() != dollar(t, "1") || s.Zurueckgelegt != 0 {
		t.Errorf("nach der Abrechnung: verfuegbar=%s zurueckgelegt=%s", s.Verfuegbar(), s.Zurueckgelegt)
	}
}

// EIN TEURERER AUSGANG ALS GESCHAETZT DARF DEN TOPF UEBERZIEHEN -- und das ist Absicht.
// Die Kosten sind bei OpenRouter bereits angefallen; sie nicht zu buchen hiesse, sie zu
// verschenken. Der Topf geht dann ins Minus und die naechste Anfrage bekommt 402.
func TestEineTeurereAntwortDarfInsMinusFuehren(t *testing.T) {
	tp := Neu()
	tp.Setze("k1", dollar(t, "0.01"))
	q, err := tp.Zuruecklegen("k1", dollar(t, "0.01"))
	if err != nil {
		t.Fatal(err)
	}
	tp.Abrechnen(q, dollar(t, "0.05")) // fuenfmal so teuer wie geschaetzt

	s, _ := tp.Stand("k1")
	if s.Rest() >= 0 {
		t.Errorf("Rest %s — die tatsaechlichen Kosten wurden nicht gebucht", s.Rest())
	}
	if _, err := tp.Zuruecklegen("k1", 1); !errors.Is(err, ErrLeer) {
		t.Error("der ueberzogene Topf traegt weiter Anfragen")
	}
}
