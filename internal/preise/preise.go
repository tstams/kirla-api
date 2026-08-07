// Paket preise haelt die Modellpreise von OpenRouter, damit sich vor dem Weiterreichen
// schaetzen laesst, was ein Aufruf kosten wird.
//
// Die Schaetzung ist ein WAECHTER und keine Rechnung (Auftrag §4): sie entscheidet nur, ob
// zurueckgelegt werden kann oder ob sofort 402 kommt. Abgerechnet wird hinterher mit dem,
// was OpenRouter tatsaechlich meldet. Sie darf also grob sein -- aber sie darf nicht zu
// NIEDRIG sein, sonst ist der 402 wirkungslos und der Topf geht ins Minus.
//
// Geholt wird die Liste beim Start und danach im Hintergrund, nie im Anfrageweg.
package preise

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tstams/kirla-api/internal/geld"
)

// Preis eines Modells, je Token.
type Preis struct {
	Eingabe geld.Betrag
	Ausgabe geld.Betrag
}

// Liste ist die Preistafel.
type Liste struct {
	mu     sync.RWMutex
	nach   map[string]Preis
	geholt time.Time
}

func Neu() *Liste { return &Liste{nach: map[string]Preis{}} }

// Stand sagt, wie viele Preise bekannt sind und wann sie geholt wurden.
func (l *Liste) Stand() (int, time.Time) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.nach), l.geholt
}

// Preis gibt den Preis eines Modells.
func (l *Liste) Preis(modell string) (Preis, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	p, da := l.nach[modell]
	return p, da
}

// antwort ist der Ausschnitt von GET /v1/models, auf den es ankommt.
type antwort struct {
	Data []struct {
		ID      string `json:"id"`
		Pricing struct {
			Prompt     string `json:"prompt"`
			Completion string `json:"completion"`
		} `json:"pricing"`
	} `json:"data"`
}

// Hole laedt die Preistafel neu.
func (l *Liste) Hole(ctx context.Context, client *http.Client, basisURL, schluessel string) error {
	url := strings.TrimSuffix(basisURL, "/") + "/models"
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+schluessel)

	antw, err := client.Do(r)
	if err != nil {
		return fmt.Errorf("modelle holen: %w", err)
	}
	defer antw.Body.Close()
	if antw.StatusCode != http.StatusOK {
		return fmt.Errorf("modelle holen: HTTP %d", antw.StatusCode)
	}

	var a antwort
	if err := json.NewDecoder(antw.Body).Decode(&a); err != nil {
		return fmt.Errorf("modelliste lesen: %w", err)
	}

	neu := make(map[string]Preis, len(a.Data))
	for _, m := range a.Data {
		// Ein unlesbarer Preis wird UEBERGANGEN und nicht als null uebernommen: ein
		// Modell mit Preis null waere gratis, und genau so entsteht eine Rechnung, die
		// niemand erwartet hat. Wer hier fehlt, bekommt spaeter den Boden.
		ein, err1 := geld.AusText(m.Pricing.Prompt)
		aus, err2 := geld.AusText(m.Pricing.Completion)
		if err1 != nil || err2 != nil {
			continue
		}
		neu[m.ID] = Preis{Eingabe: ein, Ausgabe: aus}
	}
	if len(neu) == 0 {
		return fmt.Errorf("modelliste war leer")
	}

	l.mu.Lock()
	l.nach = neu
	l.geholt = time.Now()
	l.mu.Unlock()
	return nil
}

// Zeichen je Token fuer die grobe Schaetzung der Eingabe. Vier ist die uebliche Faustregel
// fuer englischen und deutschen Text; sie unterschaetzt bei Code und Sonderzeichen. Weil
// die Schaetzung nicht zu niedrig sein darf, wird bewusst nach OBEN gerundet und der
// Aufschlag der Sicherheit unten drauf gelegt.
const zeichenJeToken = 4

// VorgabeAusgabe wird angesetzt, wenn die Anfrage kein max_tokens nennt. OpenRouter
// begrenzt dann selbst; die Zahl ist eine Annahme fuer den Waechter, keine Grenze.
const VorgabeAusgabe = 4096

// Sicherheitsaufschlag auf die Schaetzung, in Hundertstel Prozent. Die Schaetzung ist
// bewusst zu hoch statt zu niedrig -- was zu viel zurueckgelegt wurde, kommt nach der
// Antwort zurueck in den Topf.
const sicherheit = 5000 // 50 %

// Schaetze rechnet, was ein Aufruf hoechstens kosten duerfte.
//
// Ist das Modell unbekannt, kommt `false` zurueck und der Aufrufer nimmt den Boden. Eine
// Schaetzung von null waere hier der teure Fehler: sie liesse jeden Aufruf durch.
func (l *Liste) Schaetze(modell string, koerperBytes int, maxAusgabe int) (geld.Betrag, bool) {
	p, da := l.Preis(modell)
	if !da {
		return 0, false
	}
	if maxAusgabe <= 0 {
		maxAusgabe = VorgabeAusgabe
	}
	tokensEin := int64(koerperBytes+zeichenJeToken-1) / zeichenJeToken
	roh := p.Eingabe*geld.Betrag(tokensEin) + p.Ausgabe*geld.Betrag(maxAusgabe)
	return roh.MitAufschlag(sicherheit), true
}
