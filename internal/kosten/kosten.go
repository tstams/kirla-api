// Paket kosten liest aus einer OpenRouter-Antwort heraus, was sie gekostet hat -- ohne
// sie zu veraendern.
//
// DAS IST DER KERN DES PROBLEMS: der Auftrag §1 verlangt, dass die Antwort UNVERAENDERT
// beim Kunden ankommt, und §4 verlangt, dass wir aus derselben Antwort `usage.cost` lesen.
// Beides geht nur, wenn die Bytes durchlaufen und NEBENHER mitgelesen werden -- nicht,
// indem man sie erst einliest, auswertet und dann neu schreibt.
//
// Bei "stream": true kommt `usage` erst im letzten Teilstueck des text/event-stream. Der
// Mitleser merkt sich deshalb den Schwanz der Antwort und wertet ihn aus, wenn der Strom
// zu Ende ist.
package kosten

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/tstams/kirla-api/internal/geld"
)

// Quelle sagt, WOHER die Kostenzahl kommt. Der Auftrag §4 verlangt einen Boden, wenn
// OpenRouter keine Kosten meldet -- aber "kein Feld" und "Feld steht auf null" sind nicht
// dasselbe. Gemessen am 07.08.2026: inclusionai/ling-3.0-tiny:free meldet ausdruecklich
// cost=0. Wer dafuer einen Boden bucht, stellt Gratismodelle in Rechnung.
const (
	Gemeldet = "gemeldet" // usage.cost war da und groesser als null
	Frei     = "frei"     // usage.cost war da und genau null
	Boden    = "boden"    // usage.cost fehlte
)

// Verbrauch ist, was sich aus einer Antwort herauslesen laesst.
type Verbrauch struct {
	Modell    string
	TokensEin int
	TokensAus int
	Einkauf   geld.Betrag
	Quelle    string
}

// Mitleser schreibt jedes Byte unveraendert weiter und behaelt eine Kopie des Schwanzes.
//
// Der Schwanz genuegt: sowohl bei JSON als auch bei text/event-stream steht `usage` am
// Ende. Eine unbegrenzte Kopie waere der Fehler -- sie haenge den Speicher an die Laenge
// der Antwort, und genau das soll der Vorbau nicht.
type Mitleser struct {
	ziel   io.Writer
	puffer []byte
	max    int
	Gesamt int64
}

// NeuerMitleser schreibt nach ziel und behaelt hoechstens max Bytes vom Ende.
func NeuerMitleser(ziel io.Writer, max int) *Mitleser {
	if max <= 0 {
		max = 256 * 1024
	}
	return &Mitleser{ziel: ziel, max: max}
}

func (m *Mitleser) Write(p []byte) (int, error) {
	// ZUERST weiterschreiben. Ein Fehler beim Mitlesen darf den Kunden nichts kosten;
	// ein Fehler beim Schreiben beendet beides.
	n, err := m.ziel.Write(p)
	if n > 0 {
		m.Gesamt += int64(n)
		m.puffer = append(m.puffer, p[:n]...)
		if len(m.puffer) > m.max {
			// Nur den Schwanz behalten. copy statt slice, damit der grosse
			// Ausgangspuffer freigegeben werden kann.
			behalten := m.puffer[len(m.puffer)-m.max:]
			m.puffer = append(m.puffer[:0], behalten...)
		}
	}
	return n, err
}

// Schwanz gibt die behaltenen Bytes.
func (m *Mitleser) Schwanz() []byte { return m.puffer }

// huelle ist der Ausschnitt einer OpenRouter-Antwort, auf den es hier ankommt.
type huelle struct {
	Model string `json:"model"`
	Usage *struct {
		PromptTokens     int             `json:"prompt_tokens"`
		CompletionTokens int             `json:"completion_tokens"`
		Cost             json.RawMessage `json:"cost"`
	} `json:"usage"`
}

// Lies wertet den mitgelesenen Schwanz aus.
//
// stroemend entscheidet nur, WIE gesucht wird -- bei einem Strom stehen viele
// JSON-Stuecke hintereinander, bei einer gewoehnlichen Antwort genau eines.
func Lies(roh []byte, stroemend bool) Verbrauch {
	if stroemend {
		return ausStrom(roh)
	}
	return ausGanzem(roh)
}

func ausGanzem(roh []byte) Verbrauch {
	var h huelle
	if err := json.Unmarshal(bytes.TrimSpace(roh), &h); err != nil {
		// Kein lesbares JSON -- etwa weil der Schwanz mitten hineinschneidet oder die
		// Antwort ein Fehler war. Der Boden ist die richtige Annahme: sie ist nie zu
		// niedrig, und null waere geraten.
		return Verbrauch{Quelle: Boden}
	}
	return ausHuelle(h)
}

// ausStrom sucht das LETZTE data:-Stueck mit einem usage-Block.
//
// OpenRouter schickt `usage` im letzten Teilstueck vor `[DONE]`. Frueher gefundene
// Stuecke sind Zwischenstaende und wuerden zu niedrig buchen.
func ausStrom(roh []byte) Verbrauch {
	zeilen := bytes.Split(roh, []byte("\n"))
	for i := len(zeilen) - 1; i >= 0; i-- {
		z := bytes.TrimSpace(zeilen[i])
		nutz, gefunden := bytes.CutPrefix(z, []byte("data:"))
		if !gefunden {
			continue
		}
		nutz = bytes.TrimSpace(nutz)
		if len(nutz) == 0 || bytes.Equal(nutz, []byte("[DONE]")) {
			continue
		}
		var h huelle
		if err := json.Unmarshal(nutz, &h); err != nil {
			continue
		}
		if h.Usage == nil {
			continue
		}
		return ausHuelle(h)
	}
	return Verbrauch{Quelle: Boden}
}

func ausHuelle(h huelle) Verbrauch {
	v := Verbrauch{Modell: h.Model, Quelle: Boden}
	if h.Usage == nil {
		return v
	}
	v.TokensEin = h.Usage.PromptTokens
	v.TokensAus = h.Usage.CompletionTokens

	roh := strings.TrimSpace(string(h.Usage.Cost))
	if roh == "" || roh == "null" {
		// Das Feld fehlt oder ist ausdruecklich null -> Boden.
		return v
	}
	betrag, err := geld.AusText(strings.Trim(roh, `"`))
	if err != nil {
		return v
	}
	v.Einkauf = betrag
	if betrag == 0 {
		v.Quelle = Frei
	} else {
		v.Quelle = Gemeldet
	}
	return v
}

// IstStrom sagt, ob die Antwort ein text/event-stream ist.
func IstStrom(inhaltstyp string) bool {
	return strings.HasPrefix(strings.TrimSpace(strings.ToLower(inhaltstyp)), "text/event-stream")
}
