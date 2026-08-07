// Paket vorbau reicht OpenAI-kompatible Anfragen an OpenRouter weiter und tauscht dabei
// den Schluessel.
//
// Die eine Regel, aus der alles andere folgt (Auftrag §0): DER HEISSE WEG DARF NICHTS
// ANFASSEN MUESSEN. Kein Datenbankschreibvorgang, kein Stripe-Aufruf, keine Migration, die
// ihn anhaelt. Was hier steht, kommt ohne alles dahinter aus.
//
// Die zweite Regel (Auftrag §1, klabs ADR-0016): UNVERAENDERT heisst woertlich.
// Werkzeugdefinitionen, tool_choice, response_format, Streaming, Modellname — alles bleibt.
// klabs schickt Werkzeuge als `fs_read` und erwartet sie genauso zurueck; wer hier
// normalisiert, bricht die Namensbruecke im Adapter.
package vorbau

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/tstams/kirla-api/internal/schluessel"
)

// Vorbau ist der Dienst.
type Vorbau struct {
	// Ziel ist die Basis von OpenRouter, z. B. https://openrouter.ai/api/v1
	Ziel *url.URL
	// Hauptschluessel wird bei JEDER Anfrage frisch geholt, nicht einmal beim Start
	// eingefroren. Dann tauscht ein Austausch der Datei den Schluessel, ohne den Dienst
	// neu zu starten — und ein Neustart ist genau das, was man bei einem abgelaufenen
	// Schluessel nicht will.
	Hauptschluessel func() string
	Verzeichnis     *schluessel.Verzeichnis
	Client          *http.Client
	Protokoll       *slog.Logger
}

// hopfenweise sind die Kopfzeilen, die nur fuer EINE Verbindung gelten und deshalb nicht
// weitergereicht werden duerfen (RFC 9110 §7.6.1). Wer sie mitschickt, baut einen Vorbau,
// der bei aktiviertem keep-alive gelegentlich Antworten vertauscht.
var hopfenweise = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func (v *Vorbau) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	anfrageID := neueAnfrageID()
	w.Header().Set("X-Kirla-Anfrage-Id", anfrageID)

	// GET /v1/route gehoert UNS und nicht OpenRouter (Auftrag §5). Es wird in Schritt 4
	// gebaut; bis dahin muss es hier stehen, sonst reichen wir es weiter und der Kunde
	// bekommt ein 404 von einem fremden Haus, das er nicht kennt.
	if r.URL.Path == "/v1/route" {
		v.fehler(w, r, http.StatusNotImplemented, "kirla_route_noch_nicht_da",
			"Die Auskunft ueber Speicherung und Aufbewahrung gibt es noch nicht. Bis dahin steht sie in den Nutzungsbedingungen.", anfrageID)
		return
	}

	// ── /v1/key DARF NIEMALS DURCHGEREICHT WERDEN ──────────────────────────────────────
	//
	// Gefunden am 07.08.2026 beim Umstellen der ersten Instanz. klabs ruft diesen Endpunkt
	// aus `llm-budget`, um den anbieterseitigen Ausgabendeckel zu pruefen -- die Bedingung,
	// an der seine ganze Autonomie haengt ("Autonomie nur gegen einen verifizierten
	// anbieterseitigen Ausgabendeckel").
	//
	// Durchgereicht bekaeme OpenRouter den HAUPTSCHLUESSEL zu sehen und antwortete mit dem
	// Zustand des BETREIBERKONTOS: dessen Verbrauch, dessen Limit, dessen Restguthaben.
	// Zwei Schaeden auf einmal -- der Kunde saehe die Zahlen aller anderen Kunden, und
	// klabs hielte das gesamte Betreiberkonto fuer sein eigenes Limit und liefe autonom
	// gegen eine Grenze, die ihm nicht gehoert.
	//
	// Ab Schritt 2 antwortet dieser Endpunkt mit dem GUTHABENTOPF DIESES SCHLUESSELS, in
	// der Form, die OpenRouter liefert. Dann erfuellt kirla.ai die Bedingung von klabs
	// unveraendert und ohne eine Zeile Aenderung auf der Kundenseite. Bis dahin: eine
	// ehrliche Absage statt einer fremden Zahl.
	if r.URL.Path == "/v1/key" {
		v.fehler(w, r, http.StatusNotImplemented, "kirla_guthaben_noch_nicht_da",
			"Das Guthaben je Schluessel gibt es noch nicht; bis dahin kann kirla.ai keinen Ausgabendeckel ausweisen. Der Deckel des Betreiberkontos wird bewusst NICHT gemeldet.", anfrageID)
		return
	}

	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		v.fehler(w, r, http.StatusNotFound, "kirla_unbekannter_pfad",
			"Diesen Pfad gibt es hier nicht. kirla.ai spricht die OpenAI-Schnittstelle unter /v1/.", anfrageID)
		return
	}

	eintrag, err := v.pruefeSchluessel(r)
	if err != nil {
		// Gesperrt und unbekannt bekommen dieselbe Auskunft und denselben Code (Auftrag
		// §3): der Kunde soll den Betreiber fragen, und ein Fremder soll nicht erfahren,
		// welche Schluessel es gibt.
		v.fehler(w, r, http.StatusUnauthorized, "kirla_key_ungueltig",
			"Der Zugangsschluessel gilt hier nicht. Einen gueltigen Schluessel gibt es unter https://kirla.ai; danach CEO_LLM_API_KEY neu setzen.", anfrageID)
		v.Protokoll.Info("schluessel abgelehnt",
			"anfrage_id", anfrageID, "grund", err.Error(), "pfad", r.URL.Path)
		return
	}

	v.durchreichen(w, r, eintrag, anfrageID)
}

func (v *Vorbau) pruefeSchluessel(r *http.Request) (schluessel.Eintrag, error) {
	kopf := r.Header.Get("Authorization")
	klartext, gefunden := strings.CutPrefix(kopf, "Bearer ")
	if !gefunden {
		return schluessel.Eintrag{}, schluessel.ErrUngueltig
	}
	return v.Verzeichnis.Pruefe(strings.TrimSpace(klartext))
}

func (v *Vorbau) durchreichen(w http.ResponseWriter, r *http.Request, e schluessel.Eintrag, anfrageID string) {
	ziel := *v.Ziel
	// Der Pfad hinter /v1 bleibt, wie er kam. Wer hier eine Landkarte erlaubter Pfade
	// einzieht, muss sie pflegen, sobald OpenRouter etwas hinzufuegt — und bis dahin
	// bekommt der Kunde ein 404 von uns statt einer Antwort.
	ziel.Path = strings.TrimSuffix(ziel.Path, "/") + strings.TrimPrefix(r.URL.Path, "/v1")
	ziel.RawQuery = r.URL.RawQuery

	// r.Body wird NICHT gepuffert, sondern durchgereicht. Ein Vorbau, der die Anfrage
	// erst einliest, haelt sie im Speicher und verliert die Eigenschaft, die ihn billig
	// macht (Auftrag §3).
	aussen, err := http.NewRequestWithContext(r.Context(), r.Method, ziel.String(), r.Body)
	if err != nil {
		v.fehler(w, r, http.StatusInternalServerError, "kirla_anfrage_unbaubar",
			"Die Anfrage liess sich nicht weiterreichen. Bitte melden Sie das mit der Anfrage-Id.", anfrageID)
		return
	}
	uebernimmKopfzeilen(aussen.Header, r.Header)
	// Der Tausch. Mehr passiert mit dem Schluessel nicht.
	aussen.Header.Set("Authorization", "Bearer "+v.Hauptschluessel())
	aussen.ContentLength = r.ContentLength

	antw, err := v.Client.Do(aussen)
	if err != nil {
		// Ein abgebrochener Kunde ist kein Fehler des Dienstes — dann ist niemand mehr
		// da, der eine Antwort liest.
		if errors.Is(err, r.Context().Err()) {
			return
		}
		v.Protokoll.Error("openrouter nicht erreicht",
			"anfrage_id", anfrageID, "nutzer", e.Nutzer, "fehler", err)
		v.fehler(w, r, http.StatusServiceUnavailable, "kirla_route_gestoert",
			"Die Verbindung zu kirla.ai steht, der Weg zum Modellanbieter nicht. Der naechste Versuch kann gelingen.", anfrageID)
		return
	}
	defer antw.Body.Close()

	uebernimmKopfzeilen(w.Header(), antw.Header)
	// Nach WriteHeader laesst sich keine Kopfzeile mehr setzen. X-Kirla-Kosten-USD und
	// X-Kirla-Guthaben-USD stehen deshalb NICHT hier: bei "stream": true kommen die
	// Kosten erst mit dem letzten Teilstueck, also lange nach dem Kopf. Das ist in
	// Schritt 2 aufzuloesen und in docs/adr/0002 festgehalten.
	w.WriteHeader(antw.StatusCode)
	stroeme(w, antw.Body)

	v.Protokoll.Info("durchgereicht",
		"anfrage_id", anfrageID, "nutzer", e.Nutzer, "kennung", e.Kennung,
		"pfad", r.URL.Path, "status", antw.StatusCode)
}

// stroeme schiebt die Antwort weiter und leert den Puffer nach JEDEM Stueck.
//
// Ohne das Leeren sammelt Go bis zu 4 KB, bevor etwas auf die Leitung geht. Bei
// text/event-stream heisst das: der Kunde sieht minutenlang nichts und dann alles auf
// einmal. Streaming ist Pflicht, nicht Kuer (Auftrag §3).
func stroeme(w http.ResponseWriter, quelle io.Reader) {
	steuerung := http.NewResponseController(w)
	puffer := make([]byte, 8*1024)
	for {
		gelesen, leseFehler := quelle.Read(puffer)
		if gelesen > 0 {
			if _, schreibFehler := w.Write(puffer[:gelesen]); schreibFehler != nil {
				return
			}
			// Ein Fehler beim Leeren heisst, dass der Empfaenger weg ist.
			if err := steuerung.Flush(); err != nil {
				return
			}
		}
		if leseFehler != nil {
			return
		}
	}
}

// uebernimmKopfzeilen kopiert alles ausser den verbindungsgebundenen Zeilen.
func uebernimmKopfzeilen(ziel, quelle http.Header) {
	// Was in Connection genannt ist, gilt ebenfalls nur fuer diese Verbindung.
	genannt := map[string]bool{}
	for _, wert := range quelle.Values("Connection") {
		for _, name := range strings.Split(wert, ",") {
			if name = strings.TrimSpace(name); name != "" {
				genannt[http.CanonicalHeaderKey(name)] = true
			}
		}
	}
	for name, werte := range quelle {
		if genannt[name] {
			continue
		}
		if istHopfenweise(name) {
			continue
		}
		for _, wert := range werte {
			ziel.Add(name, wert)
		}
	}
}

func istHopfenweise(name string) bool {
	for _, h := range hopfenweise {
		if strings.EqualFold(name, h) {
			return true
		}
	}
	return false
}

// fehlerKoerper ist die Gestalt, die klabs erwartet (Auftrag §3).
type fehlerKoerper struct {
	Error fehlerInhalt `json:"error"`
}

type fehlerInhalt struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	AnfrageID string `json:"anfrage_id"`
}

// fehler schreibt eine Auskunft, die einem MENSCHEN gezeigt wird.
//
// Sie soll sagen, was zu tun ist, nicht was passiert ist (Auftrag §3). "invalid api key"
// laesst einen Betreiber raten; "Schluessel unter https://kirla.ai holen" nicht.
func (v *Vorbau) fehler(w http.ResponseWriter, r *http.Request, code int, kennung, text, anfrageID string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(fehlerKoerper{Error: fehlerInhalt{
		Code: kennung, Message: text, AnfrageID: anfrageID,
	}})
}

func neueAnfrageID() string {
	roh := make([]byte, 8)
	if _, err := rand.Read(roh); err != nil {
		// Eine Kennung ist eine Bequemlichkeit fuer die Rueckfrage; ohne sie faellt der
		// Dienst nicht aus.
		return "unbekannt"
	}
	return hex.EncodeToString(roh)
}
