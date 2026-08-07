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
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tstams/kirla-api/internal/ablage"
	"github.com/tstams/kirla-api/internal/geld"
	"github.com/tstams/kirla-api/internal/kosten"
	"github.com/tstams/kirla-api/internal/preise"
	"github.com/tstams/kirla-api/internal/schluessel"
	"github.com/tstams/kirla-api/internal/topf"
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

	// ── Abrechnung (Schritt 2). Ist Toepfe nil, reicht der Vorbau nur durch. ──────────
	//
	// Alles hier steht im Arbeitsspeicher. Der Anfrageweg fasst die Datenbank NICHT an
	// (Auftrag §0); die Kopfdaten gehen in einen Kanal, den ein eigener Faden leert.
	Toepfe *topf.Toepfe
	Preise *preise.Liste
	// Buch nimmt die Kopfdaten entgegen. Der Kanal ist gepuffert; laeuft er voll, wird
	// der Satz verworfen und laut protokolliert -- lieber eine Luecke in der Abrechnung
	// als ein blockierter Anfrageweg. Ab Schritt 3 faengt eine Datei das auf.
	Buch chan<- ablage.Aufruf
	// Aufschlag in Hundertstel Prozent (1500 = 15 %).
	Aufschlag int64
	// Boden wird gebucht, wenn OpenRouter keine Kosten meldet (Auftrag §4: "wird ein
	// Boden gebucht und nicht null").
	Boden geld.Betrag
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
		v.fehler(w, http.StatusNotImplemented, "kirla_route_noch_nicht_da",
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
	// Seit Schritt 2 antwortet er stattdessen mit dem GUTHABENTOPF DIESES SCHLUESSELS, in
	// genau der Form, die OpenRouter liefert -- siehe unten, hinter der Schluesselpruefung.
	// Damit erfuellt kirla.ai die Kernbedingung von klabs ("Autonomie nur gegen einen
	// verifizierten anbieterseitigen Ausgabendeckel") ohne eine Zeile Aenderung auf der
	// Kundenseite.

	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		v.fehler(w, http.StatusNotFound, "kirla_unbekannter_pfad",
			"Diesen Pfad gibt es hier nicht. kirla.ai spricht die OpenAI-Schnittstelle unter /v1/.", anfrageID)
		return
	}

	eintrag, err := v.pruefeSchluessel(r)
	if err != nil {
		// Gesperrt und unbekannt bekommen dieselbe Auskunft und denselben Code (Auftrag
		// §3): der Kunde soll den Betreiber fragen, und ein Fremder soll nicht erfahren,
		// welche Schluessel es gibt.
		v.fehler(w, http.StatusUnauthorized, "kirla_key_ungueltig",
			"Der Zugangsschluessel gilt hier nicht. Einen gueltigen Schluessel gibt es unter https://kirla.ai; danach CEO_LLM_API_KEY neu setzen.", anfrageID)
		v.Protokoll.Info("schluessel abgelehnt",
			"anfrage_id", anfrageID, "grund", err.Error(), "pfad", r.URL.Path)
		return
	}

	if r.URL.Path == "/v1/key" {
		v.guthabenAuskunft(w, eintrag, anfrageID)
		return
	}

	v.durchreichen(w, r, eintrag, anfrageID)
}

// schluesselAuskunft ist die Gestalt, die OpenRouter unter GET /v1/key liefert. klabs
// liest genau diese Felder (`llm-budget`, /usr/share/klabs/org/bin/llm-budget:55 ff.).
type schluesselAuskunft struct {
	Data struct {
		Label          string   `json:"label"`
		Usage          float64  `json:"usage"`
		Limit          *float64 `json:"limit"`
		LimitRemaining *float64 `json:"limit_remaining"`
		IsFreeTier     bool     `json:"is_free_tier"`
	} `json:"data"`
}

// guthabenAuskunft meldet den Topf DIESES Schluessels -- und niemals das Betreiberkonto.
//
// Die Abbildung ist bewusst einfach: der Topf IST der Deckel. `limit` ist das Guthaben,
// `usage` das seither Verbrauchte, `limit_remaining` die Differenz. Damit ist die Auskunft
// in sich stimmig (limit - usage = remaining) und klabs bekommt ein Limit, das NICHT null
// ist -- `limit=null` nennt llm-budget ausdruecklich den gefaehrlichsten Zustand.
//
// Gleitkomma ist hier in Ordnung und nur hier: das ist die Anzeige, nicht die Rechnung.
func (v *Vorbau) guthabenAuskunft(w http.ResponseWriter, e schluessel.Eintrag, anfrageID string) {
	if v.Toepfe == nil {
		v.fehler(w, http.StatusNotImplemented, "kirla_guthaben_noch_nicht_da",
			"Diese Zwischenstelle fuehrt keine Guthabentoepfe. Der Deckel des Betreiberkontos wird bewusst NICHT gemeldet.", anfrageID)
		return
	}
	s, da := v.Toepfe.Stand(e.Kennung)
	if !da {
		v.fehler(w, http.StatusNotImplemented, "kirla_guthaben_noch_nicht_da",
			"Zu diesem Schluessel gibt es noch keinen Guthabentopf. Bitte beim Betreiber melden.", anfrageID)
		return
	}

	var a schluesselAuskunft
	a.Data.Label = "kirla.ai " + e.Kennung
	a.Data.Usage = s.Verbraucht.Dollar()
	grenze := s.Guthaben.Dollar()
	rest := s.Rest().Dollar()
	a.Data.Limit = &grenze
	a.Data.LimitRemaining = &rest

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Kirla-Guthaben-USD", s.Rest().Text(6))
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(a)
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
	begonnen := time.Now()

	ziel := *v.Ziel
	// Der Pfad hinter /v1 bleibt, wie er kam. Wer hier eine Landkarte erlaubter Pfade
	// einzieht, muss sie pflegen, sobald OpenRouter etwas hinzufuegt — und bis dahin
	// bekommt der Kunde ein 404 von uns statt einer Antwort.
	ziel.Path = strings.TrimSuffix(ziel.Path, "/") + strings.TrimPrefix(r.URL.Path, "/v1")
	ziel.RawQuery = r.URL.RawQuery

	// Der Anfang des Koerpers wird gelesen, um Modell und max_tokens zu erfahren; danach
	// laeuft ALLES weiter durch. Gepuffert wird er nicht (Auftrag §3).
	sicht, koerper := spaehe(r.Body)

	// ── ZURUECKLEGEN, bevor bei OpenRouter Kosten entstehen (Auftrag §4) ──────────────
	var quittung *topf.Quittung
	if v.Toepfe != nil {
		var err error
		quittung, err = v.legeZurueck(e, sicht, r.ContentLength)
		if err != nil {
			// Die Ruecklage ist gescheitert — es wird nichts weitergereicht, also kann
			// auch nichts kosten.
			v.buche(ablage.Aufruf{
				AnfrageID: anfrageID, Kennung: e.Kennung, Nutzer: e.Nutzer,
				Modell: sicht.Modell, Zeit: begonnen, Dauer: time.Since(begonnen),
				Status: http.StatusPaymentRequired, Stroemend: sicht.Stroemend,
				KostenQuelle: kosten.Frei,
			})
			v.Protokoll.Warn("guthaben leer",
				"anfrage_id", anfrageID, "nutzer", e.Nutzer, "kennung", e.Kennung, "grund", err.Error())
			v.fehler(w, http.StatusPaymentRequired, "kirla_guthaben_leer",
				"Das Guthaben dieses Schluessels ist aufgebraucht. Aufladen unter https://kirla.ai — bis dahin antwortet die Schnittstelle nicht mehr.", anfrageID)
			return
		}
	}
	// Ab hier MUSS die Quittung aufgeloest werden, auf jedem Weg aus dieser Funktion.
	abgerechnet := false
	defer func() {
		if !abgerechnet {
			v.Toepfe.Freigeben(quittung)
		}
	}()

	aussen, err := http.NewRequestWithContext(r.Context(), r.Method, ziel.String(), koerper)
	if err != nil {
		v.fehler(w, http.StatusInternalServerError, "kirla_anfrage_unbaubar",
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
		v.fehler(w, http.StatusServiceUnavailable, "kirla_route_gestoert",
			"Die Verbindung zu kirla.ai steht, der Weg zum Modellanbieter nicht. Der naechste Versuch kann gelingen.", anfrageID)
		return
	}
	defer antw.Body.Close()

	uebernimmKopfzeilen(w.Header(), antw.Header)
	stroemend := kosten.IstStrom(antw.Header.Get("Content-Type"))

	// ── DIE KOSTENKOPFZEILEN, UND WARUM ES SIE NUR OHNE STROM GIBT ───────────────────
	//
	// Nach WriteHeader laesst sich keine Kopfzeile mehr setzen. Bei "stream": true kommt
	// `usage.cost` aber erst mit dem LETZTEN Teilstueck — Sekunden bis Minuten spaeter.
	// Dort KANN die Zahl also nicht im Kopf stehen; die drei verworfenen Auswege stehen
	// in docs/adr/0002.
	//
	// OHNE Strom geht es sehr wohl, und dann muss es auch stimmen: der Auftrag §3
	// verlangt "was im Topf DANACH uebrig ist". Dafuer wird die Antwort erst vollstaendig
	// eingelesen, abgerechnet und dann ausgeliefert. Das ist erlaubt und kein Widerspruch
	// zum Pufferverbot: das Verbot schuetzt den STROM, und hier ist keiner — die Bytes
	// gehen unveraendert hinaus, nur eine Wimper spaeter.
	// Content-Length taugt hier NICHT als Weiche: OpenRouter antwortet auch ohne Strom in
	// Stuecken, also ohne diese Kopfzeile. Entschieden wird nach der Laenge, die
	// tatsaechlich gelesen wurde.
	quelleKoerper := io.Reader(antw.Body)
	if !stroemend {
		roh, leseFehler := io.ReadAll(io.LimitReader(antw.Body, grenzeOhneStrom+1))
		if leseFehler != nil && len(roh) == 0 {
			v.Protokoll.Error("antwort nicht gelesen", "anfrage_id", anfrageID, "fehler", leseFehler)
			v.fehler(w, http.StatusServiceUnavailable, "kirla_route_gestoert",
				"Die Antwort des Modellanbieters brach ab. Der naechste Versuch kann gelingen.", anfrageID)
			return
		}
		if int64(len(roh)) <= grenzeOhneStrom {
			// Gepackte Antworten: eine ENTPACKTE KOPIE auswerten, die Originalbytes
			// gehen unveraendert hinaus. Ohne das bucht jeder Aufruf den Boden --
			// gefunden im Betrieb am 07.08.2026, siehe internal/kosten.
			verbrauch := kosten.Lies(kosten.Entpacke(roh, antw.Header.Get("Content-Encoding")), false)
			einkauf, quelle := v.einkauf(verbrauch, antw.StatusCode)
			verkauf := einkauf.MitAufschlag(v.Aufschlag)
			if v.Toepfe != nil {
				v.Toepfe.Abrechnen(quittung, verkauf)
				abgerechnet = true
				if s, da := v.Toepfe.Stand(e.Kennung); da {
					w.Header().Set("X-Kirla-Guthaben-USD", s.Rest().Text(6))
				}
				w.Header().Set("X-Kirla-Kosten-USD", verkauf.Text(6))
			}
			w.WriteHeader(antw.StatusCode)
			_, _ = w.Write(roh)
			v.beende(r, e, anfrageID, begonnen, antw.StatusCode, false, sicht, verbrauch, einkauf, verkauf, quelle)
			return
		}
		// Groesser als die Grenze: weiter wie ein Strom. Der schon gelesene Anfang wird
		// wieder davorgesetzt, damit nichts fehlt -- die Kostenkopfzeilen entfallen dann.
		quelleKoerper = io.MultiReader(bytes.NewReader(roh), antw.Body)
	}

	w.WriteHeader(antw.StatusCode)

	// Der Mitleser reicht jedes Byte unveraendert weiter und behaelt nur den Schwanz,
	// um daraus `usage` zu lesen.
	mit := kosten.NeuerMitleser(w, schwanzGrenze).Entpacke(antw.Header.Get("Content-Encoding"))
	stroeme(w, mit, quelleKoerper)

	// ── ABRECHNEN mit dem, was wirklich angefallen ist (Auftrag §4) ───────────────────
	verbrauch := kosten.Lies(mit.Schwanz(), stroemend)
	einkauf, quelle := v.einkauf(verbrauch, antw.StatusCode)
	verkauf := einkauf.MitAufschlag(v.Aufschlag)
	if v.Toepfe != nil {
		v.Toepfe.Abrechnen(quittung, verkauf)
		abgerechnet = true
	}
	v.beende(r, e, anfrageID, begonnen, antw.StatusCode, stroemend, sicht, verbrauch, einkauf, verkauf, quelle)
}

// beende bucht die Kopfdaten und schreibt die Zeile ins Protokoll -- fuer beide Wege
// dieselbe Stelle. Zwei Stellen waeren zwei Gelegenheiten, verschieden zu buchen.
func (v *Vorbau) beende(r *http.Request, e schluessel.Eintrag, anfrageID string,
	begonnen time.Time, status int, stroemend bool, sicht gespaeht,
	verbrauch kosten.Verbrauch, einkauf, verkauf geld.Betrag, quelle string) {

	modell := verbrauch.Modell
	if modell == "" {
		modell = sicht.Modell
	}
	v.buche(ablage.Aufruf{
		AnfrageID: anfrageID, Kennung: e.Kennung, Nutzer: e.Nutzer, Modell: modell,
		Zeit: begonnen, Dauer: time.Since(begonnen), Status: status,
		Stroemend: stroemend, TokensEin: verbrauch.TokensEin, TokensAus: verbrauch.TokensAus,
		Einkauf: einkauf, Verkauf: verkauf, KostenQuelle: quelle,
	})

	v.Protokoll.Info("durchgereicht",
		"anfrage_id", anfrageID, "nutzer", e.Nutzer, "kennung", e.Kennung,
		"pfad", r.URL.Path, "status", status, "modell", modell,
		"stroemend", stroemend, "dauer_ms", time.Since(begonnen).Milliseconds(),
		"rechnung", geld.Erklaerung(einkauf, v.Aufschlag), "kosten_quelle", quelle)
}

// schwanzGrenze ist, wie viel vom Ende der Antwort behalten wird, um `usage` zu finden.
// Sowohl bei JSON als auch bei text/event-stream steht es am Ende.
const schwanzGrenze = 256 * 1024

// grenzeOhneStrom ist, bis zu welcher Laenge eine NICHT stroemende Antwort vollstaendig
// eingelesen wird, um die Kosten noch in den Kopf zu bekommen. Der Auftrag §5 nennt ~15 KB
// im Mittel; 8 MB sind mehr als das Fuenfhundertfache. Alles darueber laeuft durch wie ein
// Strom -- dann fehlen die Kostenkopfzeilen, und das ist besser als ein Vorbau, dessen
// Speicher an der Laenge einer fremden Antwort haengt.
const grenzeOhneStrom = 8 << 20

// einkauf entscheidet, was gebucht wird -- und unterscheidet dabei die drei Faelle, die
// der Auftrag §4 zu einem verschmilzt.
func (v *Vorbau) einkauf(verbrauch kosten.Verbrauch, status int) (geld.Betrag, string) {
	switch {
	case status >= 400:
		// Eine abgelehnte Anfrage kostet bei OpenRouter nichts. Ihr einen Boden
		// aufzuerlegen hiesse, Fehler in Rechnung zu stellen.
		return 0, kosten.Frei
	case verbrauch.Quelle == kosten.Gemeldet:
		return verbrauch.Einkauf, kosten.Gemeldet
	case verbrauch.Quelle == kosten.Frei:
		// AUSDRUECKLICH null, nicht "unbekannt": ein kostenloses Modell. Hier den Boden
		// zu buchen hiesse, Gratismodelle in Rechnung zu stellen.
		return 0, kosten.Frei
	default:
		// Kein Kostenfeld. Der Auftrag §4: "Meldet OpenRouter keine Kosten, wird ein
		// Boden gebucht und nicht null. Sonst ist jedes Modell ohne Preisangabe gratis."
		return v.Boden, kosten.Boden
	}
}

// legeZurueck schaetzt und reserviert.
func (v *Vorbau) legeZurueck(e schluessel.Eintrag, sicht gespaeht, koerperLaenge int64) (*topf.Quittung, error) {
	laenge := int(koerperLaenge)
	if laenge < 0 {
		// Ohne Content-Length (chunked) ist die Laenge unbekannt; die Spaehgrenze ist
		// die sichere obere Annahme fuer den Waechter.
		laenge = spaehGrenze
	}
	schaetzung := v.Boden
	if v.Preise != nil {
		if s, bekannt := v.Preise.Schaetze(sicht.Modell, laenge, sicht.MaxAusgabe); bekannt {
			schaetzung = s.MitAufschlag(v.Aufschlag)
		}
	}
	return v.Toepfe.Zuruecklegen(e.Kennung, schaetzung)
}

// buche schiebt die Kopfdaten in den Kanal, OHNE zu blockieren.
//
// Ein voller Kanal heisst, dass der Schreiber nicht hinterherkommt -- etwa weil die
// Datenbank haengt. Dann wird der Satz verworfen und laut protokolliert. Das ist die
// bewusste Reihenfolge aus §0: lieber eine Luecke in der Abrechnung als ein Anfrageweg,
// der auf eine Datenbank wartet. Ab Schritt 3 faengt eine Datei diese Saetze auf.
func (v *Vorbau) buche(a ablage.Aufruf) {
	if v.Buch == nil {
		return
	}
	select {
	case v.Buch <- a:
	default:
		v.Protokoll.Error("kopfdaten verworfen — der schreiber kommt nicht nach",
			"anfrage_id", a.AnfrageID, "kennung", a.Kennung, "verkauf_nano", int64(a.Verkauf))
	}
}

// stroeme schiebt die Antwort weiter und leert den Puffer nach JEDEM Stueck.
//
// Ohne das Leeren sammelt Go bis zu 4 KB, bevor etwas auf die Leitung geht. Bei
// text/event-stream heisst das: der Kunde sieht minutenlang nichts und dann alles auf
// einmal. Streaming ist Pflicht, nicht Kuer (Auftrag §3).
// Geschrieben wird nach `ziel` (dem Mitleser, der weiter an w schreibt), geleert wird
// ueber w -- der Mitleser ist nur ein io.Writer und kennt den Puffer der Verbindung nicht.
func stroeme(w http.ResponseWriter, ziel io.Writer, quelle io.Reader) {
	steuerung := http.NewResponseController(w)
	puffer := make([]byte, 8*1024)
	for {
		gelesen, leseFehler := quelle.Read(puffer)
		if gelesen > 0 {
			if _, schreibFehler := ziel.Write(puffer[:gelesen]); schreibFehler != nil {
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
func (v *Vorbau) fehler(w http.ResponseWriter, code int, kennung, text, anfrageID string) {
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
