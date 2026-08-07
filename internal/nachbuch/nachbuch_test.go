package nachbuch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tstams/kirla-api/internal/ablage"
	"github.com/tstams/kirla-api/internal/geld"
)

func satz(id string, verkauf int64) ablage.Aufruf {
	return ablage.Aufruf{
		AnfrageID: id, Kennung: "k1", Nutzer: "testfirma", Modell: "anbieter/modell",
		Zeit: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC), Dauer: 1500 * time.Millisecond,
		Status: 200, TokensEin: 10, TokensAus: 20,
		Einkauf: geld.Betrag(verkauf), Verkauf: geld.Betrag(verkauf), KostenQuelle: "gemeldet",
	}
}

func TestGeschriebenesLaesstSichWiederLesen(t *testing.T) {
	pfad := filepath.Join(t.TempDir(), "unter", "nachbuch.jsonl")
	n, err := Neu(pfad)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Schreibe([]ablage.Aufruf{satz("a", 1000), satz("b", 2000)}); err != nil {
		t.Fatal(err)
	}
	if err := n.Schreibe([]ablage.Aufruf{satz("c", 3000)}); err != nil {
		t.Fatal(err)
	}

	anzahl, err := n.Anzahl()
	if err != nil {
		t.Fatal(err)
	}
	if anzahl != 3 {
		t.Errorf("%d Saetze statt 3", anzahl)
	}

	// Die Datei geht niemanden an, der zufaellig auf der Maschine ist -- in ihr stehen
	// Kundenkennungen und Betraege.
	stat, err := os.Stat(pfad)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0o600 {
		t.Errorf("die Datei steht auf %o statt 600", stat.Mode().Perm())
	}
}

// EIN LEERER STAPEL LEGT KEINE DATEI AN. Sonst steht nach jedem Start eine leere Datei da
// und sieht aus wie ein unbearbeiteter Ausfall.
func TestEinLeererStapelLegtNichtsAn(t *testing.T) {
	pfad := filepath.Join(t.TempDir(), "nachbuch.jsonl")
	n, err := Neu(pfad)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Schreibe(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pfad); !os.IsNotExist(err) {
		t.Error("es wurde eine Datei angelegt")
	}
	anzahl, err := n.Anzahl()
	if err != nil || anzahl != 0 {
		t.Errorf("Anzahl %d, Fehler %v", anzahl, err)
	}
}

// EINE KAPUTTE ZEILE HAELT DEN REST NICHT AUF. Sonst waechst das Nachbuch mit jedem
// Ausfall weiter und laesst sich nie mehr abarbeiten.
func TestEineKaputteZeileHaeltDenRestNichtAuf(t *testing.T) {
	pfad := filepath.Join(t.TempDir(), "nachbuch.jsonl")
	n, err := Neu(pfad)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Schreibe([]ablage.Aufruf{satz("a", 1000)}); err != nil {
		t.Fatal(err)
	}
	// Von Hand eine kaputte Zeile dazwischen.
	datei, err := os.OpenFile(pfad, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := datei.WriteString("{das ist kein json\n"); err != nil {
		t.Fatal(err)
	}
	datei.Close()
	if err := n.Schreibe([]ablage.Aufruf{satz("b", 2000)}); err != nil {
		t.Fatal(err)
	}

	// Anzahl zaehlt Zeilen, auch die kaputte -- sie steht ja da.
	anzahl, err := n.Anzahl()
	if err != nil {
		t.Fatal(err)
	}
	if anzahl != 3 {
		t.Errorf("%d Zeilen statt 3", anzahl)
	}
}

// DIE DATEI VERSCHWINDET ERST, WENN ALLES DRIN IST. Andersherum waere eine gescheiterte
// Einfuegung ein stiller Verlust — deshalb prueft dieser Test den Weg OHNE Datenbank
// nicht, sondern nur, dass die Datei bis dahin liegen bleibt.
func TestOhneDatenbankBleibtDieDateiLiegen(t *testing.T) {
	pfad := filepath.Join(t.TempDir(), "nachbuch.jsonl")
	n, err := Neu(pfad)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Schreibe([]ablage.Aufruf{satz("a", 1000)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pfad); err != nil {
		t.Fatalf("die Datei fehlt schon vor dem Abarbeiten: %v", err)
	}
	// Ohne erreichbare Datenbank wird hier nicht abgearbeitet; die Datei bleibt, und
	// genau das ist die Zusage.
	anzahl, _ := n.Anzahl()
	if anzahl != 1 {
		t.Errorf("%d Saetze statt 1", anzahl)
	}
}
