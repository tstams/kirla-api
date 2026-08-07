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
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"
	"sync"

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
//
// ── GEPACKTE ANTWORTEN ──────────────────────────────────────────────────────────────────
//
// Gefunden am 07.08.2026 im Betrieb, nachdem jeder klabs-Aufruf den Boden gebucht hatte
// statt seiner echten Kosten -- ein 400-facher Aufschlag. Der Grund: Gos http.Transport
// setzt von sich aus `Accept-Encoding: gzip`, wenn der Aufrufer den Kopf nicht selbst
// setzt. Der Vorbau reicht ihn unveraendert weiter (das MUSS er), OpenRouter antwortet
// gepackt -- und aus gepackten Bytes laesst sich kein `usage` lesen.
//
// Entpackt wird deshalb eine KOPIE, waehrend die Originalbytes unveraendert
// weiterlaufen. Das ist die einzige Loesung, die beide Zusagen haelt.
type Mitleser struct {
	ziel io.Writer

	// ZWEI PUFFER, ZWEI ZWECKE. Der Kopf ist der Mitschnitt fuer die Ablage (Auftrag §5) --
	// der Anfang einer Antwort ist das, was ein Mensch bei der Fehlersuche liest. Der
	// Schwanz ist fuer `usage`, das am ENDE steht. Bei einer gewoehnlichen Antwort von
	// ~15 KB sind beide dasselbe; erst bei einer langen fallen sie auseinander, und dann
	// braucht man beides.
	kopf       []byte
	schwanz    []byte
	maxKopf    int
	maxSchwanz int

	// Gesamt sind die durchgereichten Bytes, gelesen sind die (ggf. entpackten) Bytes,
	// die in die Puffer gingen.
	Gesamt  int64
	gelesen int64

	// Fuer gepackte Antworten: die Kopie laeuft durch eine Roehre in einen Entpacker.
	rohrEin  *io.PipeWriter
	fertig   chan struct{}
	mu       sync.Mutex
	entpackt bool
}

// NeuerMitleser schreibt nach ziel und behaelt hoechstens maxSchwanz Bytes vom Ende.
func NeuerMitleser(ziel io.Writer, maxSchwanz int) *Mitleser {
	if maxSchwanz <= 0 {
		maxSchwanz = 256 * 1024
	}
	return &Mitleser{ziel: ziel, maxSchwanz: maxSchwanz}
}

// MitMitschnitt schaltet zusaetzlich das Behalten des Anfangs ein -- fuer die Ablage.
// Ohne diesen Aufruf wird nur der Schwanz behalten und nichts abgelegt.
func (m *Mitleser) MitMitschnitt(maxKopf int) *Mitleser {
	if maxKopf > 0 {
		m.maxKopf = maxKopf
	}
	return m
}

// Entpacke schaltet das Entpacken ein. `kodierung` ist der Wert der Kopfzeile
// Content-Encoding; alles ausser gzip laesst den Mitleser unveraendert.
func (m *Mitleser) Entpacke(kodierung string) *Mitleser {
	if !strings.EqualFold(strings.TrimSpace(kodierung), "gzip") {
		return m
	}
	lesen, schreiben := io.Pipe()
	m.rohrEin = schreiben
	m.fertig = make(chan struct{})
	m.entpackt = true
	go func() {
		defer close(m.fertig)
		// Was hier schiefgeht, kostet nur die Kostenzahl und den Mitschnitt -- der Kunde
		// bekommt seine Antwort in jedem Fall. Deshalb wird der Rest immer leergelesen.
		defer io.Copy(io.Discard, lesen)

		aus, err := gzip.NewReader(lesen)
		if err != nil {
			return
		}
		defer aus.Close()
		puffer := make([]byte, 32*1024)
		for {
			n, leseFehler := aus.Read(puffer)
			if n > 0 {
				m.anhaengen(puffer[:n])
			}
			if leseFehler != nil {
				return
			}
		}
	}()
	return m
}

func (m *Mitleser) anhaengen(p []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gelesen += int64(len(p))

	if m.maxKopf > 0 && len(m.kopf) < m.maxKopf {
		platz := m.maxKopf - len(m.kopf)
		if platz > len(p) {
			platz = len(p)
		}
		m.kopf = append(m.kopf, p[:platz]...)
	}

	m.schwanz = append(m.schwanz, p...)
	if len(m.schwanz) > m.maxSchwanz {
		behalten := m.schwanz[len(m.schwanz)-m.maxSchwanz:]
		m.schwanz = append(m.schwanz[:0], behalten...)
	}
}

func (m *Mitleser) Write(p []byte) (int, error) {
	// ZUERST weiterschreiben. Ein Fehler beim Mitlesen darf den Kunden nichts kosten;
	// ein Fehler beim Schreiben beendet beides.
	n, err := m.ziel.Write(p)
	if n > 0 {
		m.Gesamt += int64(n)
		if m.entpackt {
			// Die Kopie geht in den Entpacker; ein Fehler dort beendet nur das Mitlesen.
			_, _ = m.rohrEin.Write(p[:n])
		} else {
			m.anhaengen(p[:n])
		}
	}
	return n, err
}

// schliesse wartet bei gepackten Antworten auf den Entpacker.
func (m *Mitleser) schliesse() {
	if m.entpackt {
		_ = m.rohrEin.Close()
		<-m.fertig
		m.entpackt = false
	}
}

// Schwanz gibt die behaltenen Bytes vom ENDE -- fuer `usage`.
func (m *Mitleser) Schwanz() []byte {
	m.schliesse()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.schwanz
}

// Mitschnitt gibt den behaltenen ANFANG und ob dabei gekuerzt wurde.
func (m *Mitleser) Mitschnitt() (kopf []byte, gekuerzt bool) {
	m.schliesse()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.kopf, m.maxKopf > 0 && m.gelesen > int64(m.maxKopf)
}

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

// Entpacke gibt die entpackten Bytes zurueck, wenn die Kodierung gzip ist -- sonst die
// Bytes, wie sie kamen. Fuer den nicht stroemenden Weg, wo die Antwort ohnehin vollstaendig
// vorliegt.
//
// Schlaegt das Entpacken fehl, kommen die Originalbytes zurueck. Sie sind dann zwar nicht
// lesbar, aber der Aufrufer bucht daraufhin den Boden -- und das ist die sichere Richtung.
func Entpacke(roh []byte, kodierung string) []byte {
	if !strings.EqualFold(strings.TrimSpace(kodierung), "gzip") {
		return roh
	}
	aus, err := gzip.NewReader(bytes.NewReader(roh))
	if err != nil {
		return roh
	}
	defer aus.Close()
	// Begrenzt: eine Zip-Bombe soll den Vorbau nicht umbringen.
	entpackt, err := io.ReadAll(io.LimitReader(aus, 32<<20))
	if err != nil && len(entpackt) == 0 {
		return roh
	}
	return entpackt
}
