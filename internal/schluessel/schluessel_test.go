package schluessel

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestEinErzeugterSchluesselWirdErkannt(t *testing.T) {
	klartext, e, err := Erzeuge("testfirma")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(klartext, Praefix) {
		t.Errorf("das Praefix fehlt — ein Scanner findet den Schluessel nicht: %q", klartext)
	}

	v := NeuesVerzeichnis([]Eintrag{e})
	gefunden, err := v.Pruefe(klartext)
	if err != nil {
		t.Fatalf("der eigene Schluessel gilt nicht: %v", err)
	}
	if gefunden.Nutzer != "testfirma" {
		t.Errorf("falscher Nutzer: %q", gefunden.Nutzer)
	}
}

// DER KLARTEXT DARF NIRGENDS LIEGEN — weder im Eintrag noch in der Datei (Auftrag §2).
func TestDerKlartextUeberlebtNirgends(t *testing.T) {
	klartext, e, err := Erzeuge("testfirma")
	if err != nil {
		t.Fatal(err)
	}
	_, geheimnis, _ := Zerlege(klartext)

	if strings.Contains(e.Hash, geheimnis) {
		t.Fatal("das Geheimnis steht im Hash")
	}

	datei := filepath.Join(t.TempDir(), "schluessel.json")
	if err := Sichere(datei, []Eintrag{e}); err != nil {
		t.Fatal(err)
	}
	roh, err := os.ReadFile(datei)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(roh), geheimnis) {
		t.Fatal("DAS GEHEIMNIS STEHT IN DER ABLAGE")
	}

	// Und die Ablage geht niemanden an, der zufaellig auf der Maschine ist.
	stat, err := os.Stat(datei)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0o600 {
		t.Errorf("die Ablage steht auf %o statt 600", stat.Mode().Perm())
	}
}

func TestFalschesGeheimnisWirdAbgelehnt(t *testing.T) {
	klartext, e, err := Erzeuge("testfirma")
	if err != nil {
		t.Fatal(err)
	}
	v := NeuesVerzeichnis([]Eintrag{e})

	// Richtige Kennung, veraendertes Geheimnis.
	verdreht := klartext[:len(klartext)-4] + "zzzz"
	if _, err := v.Pruefe(verdreht); !errors.Is(err, ErrUngueltig) {
		t.Fatalf("ein falsches Geheimnis kam durch: %v", err)
	}
}

// DIE SPERRE MUSS VOR DER BESTAETIGUNG GREIFEN.
//
// Die Pruefung merkt sich eine gelungene Bestaetigung, damit nicht jede Anfrage durch
// bcrypt laeuft. Wer die Sperre DANACH prueft, laesst einen gesperrten Schluessel bis zum
// Ablauf der Bestaetigung weiterlaufen — bei einem Missbrauchsfall genau die Minuten, in
// denen es darauf ankommt.
func TestDieSperreGreiftAuchNachEinerBestaetigung(t *testing.T) {
	klartext, e, err := Erzeuge("testfirma")
	if err != nil {
		t.Fatal(err)
	}
	v := NeuesVerzeichnis([]Eintrag{e})

	if _, err := v.Pruefe(klartext); err != nil {
		t.Fatalf("erste Pruefung: %v", err)
	}

	// Jetzt sperren — so, wie es ein Neuladen aus der Ablage taete.
	e.Gesperrt = true
	v.mu.Lock()
	v.nach[e.Kennung] = e
	v.mu.Unlock()

	if _, err := v.Pruefe(klartext); !errors.Is(err, ErrGesperrt) {
		t.Fatalf("der gesperrte Schluessel lief weiter: %v", err)
	}
}

// EINE ZWEITE PRUEFUNG DARF NICHT ERNEUT DURCH BCRYPT. Bei 20-30 gleichzeitigen Anfragen
// in der Spitze ist das sonst der teuerste Teil des heissen Weges.
func TestDieZweitePruefungIstBillig(t *testing.T) {
	klartext, e, err := Erzeuge("testfirma")
	if err != nil {
		t.Fatal(err)
	}
	v := NeuesVerzeichnis([]Eintrag{e})
	if _, err := v.Pruefe(klartext); err != nil {
		t.Fatal(err)
	}

	// Der Hash wird unbrauchbar gemacht. Kaeme die zweite Pruefung noch bei bcrypt an,
	// muesste sie jetzt scheitern.
	kaputt := e
	kaputt.Hash = "$2a$12$dieserhashpasstzunichtsmehrundsollteniegepruueftwerden"
	v.mu.Lock()
	v.nach[e.Kennung] = kaputt
	v.mu.Unlock()

	if _, err := v.Pruefe(klartext); err != nil {
		t.Fatalf("die zweite Pruefung lief erneut durch bcrypt: %v", err)
	}
}

func TestFremdeFormateWerdenAbgelehnt(t *testing.T) {
	_, e, err := Erzeuge("testfirma")
	if err != nil {
		t.Fatal(err)
	}
	v := NeuesVerzeichnis([]Eintrag{e})

	for _, fall := range []struct{ name, key string }{
		{"leer", ""},
		{"ein OpenRouter-Schluessel", "sk-or-v1-abcdefghijklmnop"},
		{"nur das Praefix", Praefix},
		{"ohne Trenner", Praefix + "abcdefghijklmnopqrstuvwxyz012345678901234567"},
		{"zu kurze Kennung", Praefix + "abc_" + strings.Repeat("a", geheimnisZeichen)},
		{"zu kurzes Geheimnis", Praefix + strings.Repeat("a", kennungZeichen) + "_kurz"},
	} {
		t.Run(fall.name, func(t *testing.T) {
			if _, err := v.Pruefe(fall.key); !errors.Is(err, ErrUngueltig) {
				t.Errorf("%q kam durch: %v", fall.key, err)
			}
		})
	}
}

// UNBEKANNT UND FALSCH BEKOMMEN DIESELBE AUSKUNFT. Wer sie unterscheidet, verraet einem
// Fremden, welche Kennungen es gibt.
func TestUnbekanntUndFalschSindNichtUnterscheidbar(t *testing.T) {
	klartext, e, err := Erzeuge("testfirma")
	if err != nil {
		t.Fatal(err)
	}
	v := NeuesVerzeichnis([]Eintrag{e})

	erfunden := Praefix + strings.Repeat("q", kennungZeichen) + "_" + strings.Repeat("q", geheimnisZeichen)
	_, fehlerUnbekannt := v.Pruefe(erfunden)
	_, fehlerFalsch := v.Pruefe(klartext[:len(klartext)-4] + "zzzz")

	if fehlerUnbekannt.Error() != fehlerFalsch.Error() {
		t.Errorf("die Auskuenfte unterscheiden sich: %q vs %q", fehlerUnbekannt, fehlerFalsch)
	}
}

// Geprueft wird der Zufall, NICHT Erzeuge: 20 000 Schluessel durch bcrypt zu drehen dauert
// eine Stunde und beweist nichts ueber die Kennungen.
func TestZweiKennungenKollidierenNicht(t *testing.T) {
	gesehen := map[string]bool{}
	for i := 0; i < 20000; i++ {
		k, err := zufall(kennungZeichen)
		if err != nil {
			t.Fatal(err)
		}
		if len(k) != kennungZeichen {
			t.Fatalf("Kennung hat %d Zeichen statt %d: %q", len(k), kennungZeichen, k)
		}
		if gesehen[k] {
			t.Fatalf("dieselbe Kennung zweimal nach %d Zuegen: %s", i, k)
		}
		gesehen[k] = true
	}
}

// TestMain senkt die bcrypt-Kosten fuer die Tests. Im Betrieb gilt der Wert aus dem
// Quelltext; hier zaehlt, dass die Liste in Sekunden durchlaeuft und nicht in Minuten.
func TestMain(m *testing.M) {
	kostenfaktor = bcrypt.MinCost
	os.Exit(m.Run())
}

// BenchmarkPruefungOhneBestaetigung misst, was eine frische Pruefung im Betrieb kostet —
// die Zahl, an der die Wahl des Kostenfaktors haengt.
func BenchmarkPruefungOhneBestaetigung(b *testing.B) {
	kostenfaktor = 12
	defer func() { kostenfaktor = bcrypt.MinCost }()

	klartext, e, err := Erzeuge("testfirma")
	if err != nil {
		b.Fatal(err)
	}
	_, geheimnis, _ := Zerlege(klartext)

	b.ResetTimer()
	for b.Loop() {
		if err := bcrypt.CompareHashAndPassword([]byte(e.Hash), []byte(geheimnis)); err != nil {
			b.Fatal(err)
		}
	}
}

func TestAblageUeberlebtDenUmweg(t *testing.T) {
	klartext, e, err := Erzeuge("testfirma")
	if err != nil {
		t.Fatal(err)
	}
	datei := filepath.Join(t.TempDir(), "schluessel.json")
	if err := Sichere(datei, []Eintrag{e}); err != nil {
		t.Fatal(err)
	}
	v, err := Lade(datei)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Pruefe(klartext); err != nil {
		t.Fatalf("nach dem Laden gilt der Schluessel nicht mehr: %v", err)
	}
}
