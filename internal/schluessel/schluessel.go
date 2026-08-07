// Paket schluessel prueft kirla.ai-Zugangsschluessel.
//
// Der Schluessel IST die Bindung an kirla.ai (klabs ADR-0016): der Kunde bekommt nie einen
// OpenRouter-Schluessel, und `kirla_sk_...` ist bei OpenRouter ungueltig. Hier steht deshalb
// KEINE Aktivierung, KEINE Signaturpruefung und kein Dienst, der "ist das echt?" beantwortet.
// Nur die eine Frage: gehoert dieser Schluessel einem Nutzer, und ist er offen?
package schluessel

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Praefix, an dem ein kirla.ai-Schluessel erkennbar ist. Er bleibt bewusst im Klartext:
// ein versehentlich in ein oeffentliches Repositorium geratener Schluessel soll von den
// Scannern der Anbieter gefunden werden (Auftrag §2).
const Praefix = "kirla_sk_"

// Laengen in Zeichen der base32-Darstellung. 12 Zeichen Kennung sind 60 Bit — genug, dass
// zwei Kennungen nicht kollidieren; 32 Zeichen Geheimnis sind 160 Bit.
const (
	kennungZeichen   = 12
	geheimnisZeichen = 32
)

var (
	// ErrUngueltig deckt alles ab, was kein gueltiger Schluessel ist: falsches Praefix,
	// falsche Gestalt, unbekannte Kennung, falsches Geheimnis. Die Faelle werden nach
	// aussen NICHT unterschieden — wer sie unterscheidet, verraet einem Fremden, welche
	// Kennungen es gibt.
	ErrUngueltig = errors.New("schluessel ungueltig")
	// ErrGesperrt ist ein bekannter Schluessel, den der Betreiber stillgelegt hat.
	ErrGesperrt = errors.New("schluessel gesperrt")
)

// kodierung ohne Polsterung und in Kleinbuchstaben — ein Schluessel wird von Menschen
// kopiert, und '=' am Ende ueberlebt weder Kommandozeilen noch Konfigurationsdateien
// zuverlaessig.
var kodierung = base32.StdEncoding.WithPadding(base32.NoPadding)

// Eintrag ist ein vergebener Schluessel. Das Geheimnis steht hier NICHT — nur sein Hash.
type Eintrag struct {
	Kennung  string    `json:"kennung"`
	Hash     string    `json:"hash"`
	Nutzer   string    `json:"nutzer"`
	Gesperrt bool      `json:"gesperrt"`
	Angelegt time.Time `json:"angelegt"`
}

// Erzeuge legt einen neuen Schluessel an und gibt ihn EINMAL im Klartext zurueck. Danach
// existiert er nirgends mehr; der Eintrag traegt nur den Hash (Auftrag §2).
func Erzeuge(nutzer string) (klartext string, e Eintrag, err error) {
	kennung, err := zufall(kennungZeichen)
	if err != nil {
		return "", Eintrag{}, err
	}
	geheimnis, err := zufall(geheimnisZeichen)
	if err != nil {
		return "", Eintrag{}, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(geheimnis), Kostenfaktor)
	if err != nil {
		return "", Eintrag{}, fmt.Errorf("hash erzeugen: %w", err)
	}
	return Praefix + kennung + "_" + geheimnis, Eintrag{
		Kennung:  kennung,
		Hash:     string(hash),
		Nutzer:   nutzer,
		Angelegt: time.Now().UTC(),
	}, nil
}

// kostenfaktor von bcrypt. 12 ist heute die uebliche Wahl.
//
// Der Auftrag (§2) erlaubt argon2id oder bcrypt; bcrypt ist genommen, weil es keinen
// Speicher braucht. Gemessen kostet eine frische Pruefung damit auf dieser Maschine
// 268 ms — nicht wenig, aber sie faellt dank der Bestaetigung unten nur einmal je
// Schluessel und Fenster an.
//
// WAS HIER OFFEN IST, und es gehoert dem Betreiber: ein `kirla_sk_...` ist KEIN von einem
// Menschen gewaehltes Kennwort, sondern 160 Bit Zufall. Ein langsames Verfahren schuetzt
// gegen Durchprobieren — gegen 160 Bit probiert niemand. SHA-256 waere hier ebenso sicher
// und rund 250 000-mal billiger. Der Auftrag nennt argon2id oder bcrypt, also steht bcrypt;
// die Abwaegung steht in docs/adr/0001 und ist ohne Neuvergabe von Schluesseln jederzeit
// umzustellen.
//
// Ausgeschrieben und keine Konstante, damit Tests ihn senken koennen: mit 12 kostet jede
// Erzeugung eine Viertelsekunde, und unter -race wird eine Testliste zur Kaffeepause. Das
// Paket liegt unter internal/, ist also keine oeffentliche Schnittstelle. IM BETRIEB WIRD
// ER NIE GESETZT -- wer das doch tut, senkt die Kosten fuer einen Angreifer.
var Kostenfaktor = 12

func zufall(zeichen int) (string, error) {
	// base32 macht aus 5 Bit ein Zeichen; wir brauchen aufgerundet so viele Bytes.
	roh := make([]byte, (zeichen*5+7)/8)
	if _, err := rand.Read(roh); err != nil {
		return "", fmt.Errorf("zufall: %w", err)
	}
	return strings.ToLower(kodierung.EncodeToString(roh))[:zeichen], nil
}

// Zerlege trennt einen Schluessel im Klartext in Kennung und Geheimnis.
//
// WARUM DER SCHLUESSEL EINE KENNUNG TRAEGT und nicht nur Zufall ist: ohne sie muesste die
// Pruefung jeden gespeicherten Hash durchprobieren, also je Anfrage N bcrypt-Laeufe. Bei
// einem Testnutzer faellt das nicht auf, bei hundert Kunden ist es der teuerste Teil des
// heissen Weges. Die Kennung macht daraus einen Zugriff auf eine Landkarte und genau EINEN
// bcrypt-Lauf. Das Format ist eine Einbahnstrasse — es nachtraeglich zu aendern heisst,
// jedem Kunden einen neuen Schluessel zu geben. Deshalb steht es hier von Anfang an.
func Zerlege(klartext string) (kennung, geheimnis string, ok bool) {
	rest, gefunden := strings.CutPrefix(klartext, Praefix)
	if !gefunden {
		return "", "", false
	}
	kennung, geheimnis, gefunden = strings.Cut(rest, "_")
	if !gefunden || len(kennung) != kennungZeichen || len(geheimnis) != geheimnisZeichen {
		return "", "", false
	}
	return kennung, geheimnis, true
}

// Verzeichnis haelt die vergebenen Schluessel im Arbeitsspeicher.
//
// Es wird beim Start gefuellt und danach nur gelesen. Das ist kein Zufall, sondern die
// Betriebsregel des Auftrags (§0): der heisse Weg darf nichts anfassen muessen. Faellt die
// Quelle aus, altert dieses Verzeichnis einfach — es hoert nicht auf zu antworten.
type Verzeichnis struct {
	mu         sync.RWMutex
	nach       map[string]Eintrag // Kennung -> Eintrag
	bestaetigt map[string]bestaetigung
}

// bestaetigung merkt sich eine bereits gelungene Pruefung, damit derselbe Schluessel nicht
// bei jeder Anfrage erneut durch bcrypt laeuft. Gespeichert wird der SHA-256 des Klartextes
// als Landkartenschluessel — nie der Klartext selbst, damit ein Speicherabbild ihn nicht
// preisgibt.
type bestaetigung struct {
	kennung string
	bis     time.Time
}

// bestaetigungsdauer. Kurz genug, dass eine Sperre schnell greift, lang genug, dass eine
// taktende Instanz nicht bei jedem Zug erneut zahlt.
const bestaetigungsdauer = 5 * time.Minute

// NeuesVerzeichnis baut ein Verzeichnis aus Eintraegen.
func NeuesVerzeichnis(eintraege []Eintrag) *Verzeichnis {
	v := &Verzeichnis{
		nach:       make(map[string]Eintrag, len(eintraege)),
		bestaetigt: make(map[string]bestaetigung),
	}
	for _, e := range eintraege {
		v.nach[e.Kennung] = e
	}
	return v
}

// ablage ist die Gestalt der Datei auf der Platte.
type ablage struct {
	Schluessel []Eintrag `json:"schluessel"`
}

// Lade liest das Verzeichnis aus einer Datei.
//
// Fuer Schritt 1 ist die Ablage eine Datei und keine Datenbank — Schritt 1 soll ohne
// Datenbank tragen (Auftrag §8). Ab Schritt 2 fuellt die Datenbank dieselbe Landkarte.
func Lade(pfad string) (*Verzeichnis, error) {
	roh, err := os.ReadFile(pfad)
	if err != nil {
		return nil, fmt.Errorf("schluesselablage lesen: %w", err)
	}
	var a ablage
	if err := json.Unmarshal(roh, &a); err != nil {
		return nil, fmt.Errorf("schluesselablage %s: %w", pfad, err)
	}
	return NeuesVerzeichnis(a.Schluessel), nil
}

// Sichere schreibt das Verzeichnis zurueck. Nur fuer die Vergabe von Hand — nicht im
// Anfrageweg.
func Sichere(pfad string, eintraege []Eintrag) error {
	roh, err := json.MarshalIndent(ablage{Schluessel: eintraege}, "", "  ")
	if err != nil {
		return err
	}
	// 0600: der Hauptschluessel liegt woanders, aber auch die Hashes gehen niemanden an,
	// der zufaellig auf der Maschine ist.
	return os.WriteFile(pfad, append(roh, '\n'), 0o600)
}

// Alle gibt die Eintraege zurueck; fuer die Vergabe von Hand.
func (v *Verzeichnis) Alle() []Eintrag {
	v.mu.RLock()
	defer v.mu.RUnlock()
	aus := make([]Eintrag, 0, len(v.nach))
	for _, e := range v.nach {
		aus = append(aus, e)
	}
	return aus
}

// Pruefe beantwortet, ob dieser Schluessel gilt.
//
// Die Rueckgabe unterscheidet nur zwei Faelle nach aussen: ungueltig und gesperrt. Ein
// "die Kennung gibt es, aber das Geheimnis stimmt nicht" waere eine Auskunft an jemanden,
// der sie nicht bekommen soll.
func (v *Verzeichnis) Pruefe(klartext string) (Eintrag, error) {
	kennung, geheimnis, ok := Zerlege(klartext)
	if !ok {
		return Eintrag{}, ErrUngueltig
	}

	v.mu.RLock()
	e, bekannt := v.nach[kennung]
	v.mu.RUnlock()
	if !bekannt {
		return Eintrag{}, ErrUngueltig
	}
	// Die Sperre wird VOR der Bestaetigung geprueft, sonst laesst eine noch gueltige
	// Bestaetigung einen gesperrten Schluessel bis zu ihrem Ablauf weiterlaufen.
	if e.Gesperrt {
		return Eintrag{}, ErrGesperrt
	}

	merker := landkartenschluessel(klartext)
	if v.istBestaetigt(merker, kennung) {
		return e, nil
	}

	if err := bcrypt.CompareHashAndPassword([]byte(e.Hash), []byte(geheimnis)); err != nil {
		return Eintrag{}, ErrUngueltig
	}
	v.merkeBestaetigung(merker, kennung)
	return e, nil
}

// landkartenschluessel bildet den Klartext auf einen Wert ab, der im Speicher stehen darf.
func landkartenschluessel(klartext string) string {
	summe := sha256.Sum256([]byte(klartext))
	return string(summe[:])
}

func (v *Verzeichnis) istBestaetigt(merker, kennung string) bool {
	v.mu.RLock()
	b, da := v.bestaetigt[merker]
	v.mu.RUnlock()
	if !da || time.Now().After(b.bis) {
		return false
	}
	// Der Vergleich in konstanter Zeit ist hier nicht gegen einen Angreifer gerichtet,
	// sondern gegen eine spaetere Umbenennung, die zwei Kennungen verwechselt.
	return subtle.ConstantTimeCompare([]byte(b.kennung), []byte(kennung)) == 1
}

func (v *Verzeichnis) merkeBestaetigung(merker, kennung string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	// Der Speicher waechst mit der Zahl der Schluessel, nicht der Anfragen; abgelaufene
	// Eintraege werden bei Gelegenheit entfernt, damit gesperrte nicht ewig liegen.
	jetzt := time.Now()
	for k, b := range v.bestaetigt {
		if jetzt.After(b.bis) {
			delete(v.bestaetigt, k)
		}
	}
	v.bestaetigt[merker] = bestaetigung{kennung: kennung, bis: jetzt.Add(bestaetigungsdauer)}
}
