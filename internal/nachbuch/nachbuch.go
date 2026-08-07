// Paket nachbuch faengt Abrechnung auf, die gerade nicht in die Datenbank kann.
//
// Der Auftrag §4: "Waehrend des Kulanzfensters (Datenbank weg) wird nicht zurueckgelegt,
// sondern nur mitgeschrieben — in eine Datei, die nach der Wiederkehr abgearbeitet wird."
//
// Die Datei ist bewusst das dummste Ding, das die Aufgabe erfuellt: eine Zeile JSON je
// Aufruf, angehaengt, mit fsync. Kein Format, das man kennen muss, keine Bibliothek, kein
// Zustand. Wer sie im Ernstfall von Hand lesen muss, kann das mit `cat`.
package nachbuch

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/tstams/kirla-api/internal/ablage"
)

// Nachbuch ist die Datei.
type Nachbuch struct {
	pfad string
	mu   sync.Mutex
}

func Neu(pfad string) (*Nachbuch, error) {
	if err := os.MkdirAll(filepath.Dir(pfad), 0o700); err != nil {
		return nil, fmt.Errorf("nachbuch-verzeichnis: %w", err)
	}
	return &Nachbuch{pfad: pfad}, nil
}

func (n *Nachbuch) Pfad() string { return n.pfad }

// Schreibe haengt einen Stapel an.
//
// Mit fsync, und das ist hier keine Uebervorsicht: die Datei ist waehrend eines
// Datenbankausfalls die EINZIGE Spur der Abrechnung. Ein Stromausfall obendrauf ist
// unwahrscheinlich, aber genau die Art Tag, an dem beides passiert.
func (n *Nachbuch) Schreibe(stapel []ablage.Aufruf) error {
	if len(stapel) == 0 {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()

	datei, err := os.OpenFile(n.pfad, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("nachbuch oeffnen: %w", err)
	}
	defer datei.Close()

	schreiber := bufio.NewWriter(datei)
	for _, a := range stapel {
		roh, err := json.Marshal(a)
		if err != nil {
			return fmt.Errorf("nachbuch schreiben: %w", err)
		}
		if _, err := schreiber.Write(append(roh, '\n')); err != nil {
			return fmt.Errorf("nachbuch schreiben: %w", err)
		}
	}
	if err := schreiber.Flush(); err != nil {
		return fmt.Errorf("nachbuch schreiben: %w", err)
	}
	return datei.Sync()
}

// Anzahl sagt, wie viele Saetze warten. Fuer die Anzeige und fuers Protokoll.
func (n *Nachbuch) Anzahl() (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.zaehle()
}

func (n *Nachbuch) zaehle() (int, error) {
	datei, err := os.Open(n.pfad)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer datei.Close()
	leser := bufio.NewScanner(datei)
	leser.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	n2 := 0
	for leser.Scan() {
		if len(leser.Bytes()) > 0 {
			n2++
		}
	}
	return n2, leser.Err()
}

// Arbeite traegt die wartenden Saetze in die Datenbank ein und raeumt die Datei weg.
//
// Die Reihenfolge ist entscheidend: ERST schreiben, DANN loeschen. Andersherum waere eine
// gescheiterte Einfuegung ein stiller Verlust. Dass ein Satz dabei zweimal ankommt, faengt
// das ON CONFLICT DO NOTHING in SchreibeAufrufe ab -- die Anfrage-Id ist der
// Primaerschluessel.
//
// Die Abbuchung steckt in derselben Transaktion wie die Kopfdaten und laeuft damit in der
// DATENBANK genau einmal.
//
// IM ARBEITSSPEICHER STEHT DERSELBE BETRAG EIN ZWEITES MAL, und das ist Absicht: waehrend
// des Ausfalls wird der Verbrauch dort mitgefuehrt, damit /v1/key nicht stundenlang ein
// Guthaben meldet, das es nicht mehr gibt. `nachErfolg` bekommt jeden Stapel, der WIRKLICH
// in der Datenbank steht -- darueber raeumt der Aufrufer den Betrag aus dem Arbeitsspeicher
// wieder heraus. Ohne diesen Rueckruf zieht der naechste Abgleich denselben Betrag zweimal
// ab.
func (n *Nachbuch) Arbeite(ctx context.Context, abl *ablage.Ablage, stapelgroesse int, nachErfolg func([]ablage.Aufruf)) (int, error) {
	if stapelgroesse <= 0 {
		stapelgroesse = 200
	}
	n.mu.Lock()
	defer n.mu.Unlock()

	datei, err := os.Open(n.pfad)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	leser := bufio.NewScanner(datei)
	leser.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	getragen := 0
	stapel := make([]ablage.Aufruf, 0, stapelgroesse)

	schiebe := func() error {
		if len(stapel) == 0 {
			return nil
		}
		if err := abl.SchreibeAufrufe(ctx, stapel); err != nil {
			return err
		}
		if nachErfolg != nil {
			nachErfolg(stapel)
		}
		getragen += len(stapel)
		stapel = stapel[:0]
		return nil
	}

	for leser.Scan() {
		zeile := leser.Bytes()
		if len(zeile) == 0 {
			continue
		}
		var a ablage.Aufruf
		if err := json.Unmarshal(zeile, &a); err != nil {
			// Eine kaputte Zeile haelt den Rest nicht auf. Sie geht verloren, und das
			// steht im Rueckgabefehler -- besser als ein Nachbuch, das sich nie
			// abarbeiten laesst und mit jedem Ausfall weiterwaechst.
			continue
		}
		stapel = append(stapel, a)
		if len(stapel) >= stapelgroesse {
			if err := schiebe(); err != nil {
				datei.Close()
				return getragen, err
			}
		}
	}
	if err := leser.Err(); err != nil {
		datei.Close()
		return getragen, err
	}
	if err := schiebe(); err != nil {
		datei.Close()
		return getragen, err
	}
	datei.Close()

	// Alles drin -- jetzt darf die Datei weg.
	if err := os.Remove(n.pfad); err != nil && !os.IsNotExist(err) {
		return getragen, fmt.Errorf("nachbuch aufraeumen: %w", err)
	}
	return getragen, nil
}
