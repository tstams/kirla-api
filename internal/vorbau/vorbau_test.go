package vorbau

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tstams/kirla-api/internal/schluessel"
)

const hauptschluesselTest = "sk-or-v1-nur-fuer-den-test"

// bau stellt einen Vorbau vor ein gefaelschtes OpenRouter.
func bau(t *testing.T, echo http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()

	fern := httptest.NewServer(echo)
	t.Cleanup(fern.Close)

	klartext, eintrag, err := schluessel.Erzeuge("testfirma")
	if err != nil {
		t.Fatalf("schluessel erzeugen: %v", err)
	}
	ziel, err := url.Parse(fern.URL + "/v1")
	if err != nil {
		t.Fatal(err)
	}

	v := &Vorbau{
		Ziel:            ziel,
		Hauptschluessel: func() string { return hauptschluesselTest },
		Verzeichnis:     schluessel.NeuesVerzeichnis([]schluessel.Eintrag{eintrag}),
		Client:          &http.Client{},
		Protokoll:       slog.New(slog.DiscardHandler),
	}
	vor := httptest.NewServer(v)
	t.Cleanup(vor.Close)
	return vor, klartext
}

func frage(t *testing.T, vor *httptest.Server, key, koerper string) *http.Response {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, vor.URL+"/v1/chat/completions", strings.NewReader(koerper))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	antw, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("anfrage: %v", err)
	}
	t.Cleanup(func() { antw.Body.Close() })
	return antw
}

// ── DER FALL, DEN DER AUFTRAG AUSDRUECKLICH VERLANGT ────────────────────────────────────
//
// 06.08.2026 in klabs: ein Modell schrieb eine 7,8-KB-Datei mit `fs.write` und lief in die
// Ausgabegrenze. Die Werkzeugargumente kamen mitten im JSON abgeschnitten an. klabs
// behandelt das seit 2.18.0 als Werkzeugfehler und laeuft weiter — das kann es nur, wenn
// es die kaputte Gestalt UEBERHAUPT ZU SEHEN BEKOMMT.
//
// Ein Vorbau, der hier "repariert", nimmt klabs die Information weg, aus der es lernt,
// weniger auf einmal zu schreiben. Die Bytes muessen durchgehen, wie sie kamen.
// Die Vorlage steht in klabs: core/openrouter/openrouter_test.go:496.
func TestDerAbgeschnitteneWerkzeugwunschGehtUnveraendertDurch(t *testing.T) {
	// Genau die Gestalt aus dem Protokoll: gueltige Huelle, abgeschnittenes JSON darin.
	const kaputt = `{"model":"anbieter/modell-mittel","choices":[{"message":{"tool_calls":[
	  {"id":"call_1","type":"function","function":{"name":"fs_write",
	   "arguments":"{\"path\": \"/x/gross.py\", \"content\": \"\\\"\\\"\\\"\\nOrder Manager"}}]}}],
	  "usage":{"prompt_tokens":10,"completion_tokens":2048,"cost":0.002}}`

	vor, key := bau(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, kaputt)
	})

	antw := frage(t, vor, key, `{"model":"anbieter/modell-mittel"}`)
	gelesen, _ := io.ReadAll(antw.Body)

	if string(gelesen) != kaputt {
		t.Errorf("der Vorbau hat die Antwort angefasst.\n--- angekommen:\n%s\n--- erwartet:\n%s", gelesen, kaputt)
	}
	if antw.StatusCode != http.StatusOK {
		t.Errorf("Status %d — der Vorbau hat aus einer gueltigen Antwort einen Fehler gemacht", antw.StatusCode)
	}
}

// WIRKLICH KAPUTTES JSON ist etwas anderes als abgeschnittenes, und klabs unterscheidet
// beides (openrouter_test.go:541). Auch das ist nicht unsere Aufgabe.
func TestKaputtesJSONGehtUnveraendertDurch(t *testing.T) {
	const kaputt = `{"model":"m","choices":[{"message":{"tool_calls":[
	  {"id":"call_2","type":"function","function":{"name":"fs_read",
	   "arguments":"[\"nicht\", \"ein\", \"objekt\"]"}}]}}]}`

	vor, key := bau(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, kaputt)
	})

	antw := frage(t, vor, key, `{"model":"m"}`)
	gelesen, _ := io.ReadAll(antw.Body)
	if string(gelesen) != kaputt {
		t.Errorf("angefasst.\nangekommen: %s\nerwartet:   %s", gelesen, kaputt)
	}
}

// DIE NAMENSBRUECKE. klabs heisst im Haus `fs.read` und auf der Leitung `fs_read`; der
// Adapter bildet hin und zurueck ab. Wer hier normalisiert — etwa Punkte einsetzt, weil
// "die Spec einen Punkt verlangt" —, bricht sie in beide Richtungen.
func TestWerkzeugnamenBleibenWieSieSind(t *testing.T) {
	const hinaus = `{"model":"m","tools":[{"type":"function","function":{"name":"fs_read"}}]}`
	const zurueck = `{"choices":[{"message":{"tool_calls":[{"id":"c1","type":"function","function":{"name":"fs_read","arguments":"{}"}}]}}]}`

	var angekommen string
	vor, key := bau(t, func(w http.ResponseWriter, r *http.Request) {
		roh, _ := io.ReadAll(r.Body)
		angekommen = string(roh)
		_, _ = io.WriteString(w, zurueck)
	})

	antw := frage(t, vor, key, hinaus)
	gelesen, _ := io.ReadAll(antw.Body)

	if angekommen != hinaus {
		t.Errorf("die Anfrage kam veraendert bei OpenRouter an.\nangekommen: %s\ngeschickt:  %s", angekommen, hinaus)
	}
	if string(gelesen) != zurueck {
		t.Errorf("die Antwort kam veraendert beim Kunden an.\nangekommen: %s\nerwartet:   %s", gelesen, zurueck)
	}
}

// DER TAUSCH IST DIE GANZE AUFGABE. Der Kundenschluessel darf OpenRouter nie erreichen —
// sonst waere er dort ein Zugangsmittel, und die Bindung aus ADR-0016 loest sich auf.
func TestDerKundenschluesselErreichtOpenRouterNie(t *testing.T) {
	var gesehen string
	vor, key := bau(t, func(w http.ResponseWriter, r *http.Request) {
		gesehen = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{}`)
	})

	frage(t, vor, key, `{}`)

	if gesehen != "Bearer "+hauptschluesselTest {
		t.Errorf("OpenRouter sah %q statt des Hauptschluessels", gesehen)
	}
	if strings.Contains(gesehen, schluessel.Praefix) {
		t.Fatalf("DER KUNDENSCHLUESSEL GING HINAUS: %q", gesehen)
	}
}

// STREAMING IST PFLICHT, NICHT KUER. Der Beweis ist nicht, dass am Ende alles da ist,
// sondern dass ein Teilstueck ANKOMMT, bevor das naechste geschrieben wird. Ein Vorbau,
// der puffert, besteht einen Test, der nur das Ergebnis vergleicht.
func TestTeilstueckeKommenEinzelnAn(t *testing.T) {
	losgelassen := make(chan struct{})

	vor, key := bau(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		steuerung := http.NewResponseController(w)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"eins\"}}]}\n\n")
		_ = steuerung.Flush()

		// Das zweite Stueck erst, wenn der Kunde das erste WIRKLICH hat.
		select {
		case <-losgelassen:
		case <-time.After(5 * time.Second):
			t.Error("das erste Teilstueck kam nie beim Kunden an — der Vorbau puffert")
			return
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		_ = steuerung.Flush()
	})

	antw := frage(t, vor, key, `{"model":"m","stream":true}`)
	leser := bufio.NewReader(antw.Body)

	erste, err := leser.ReadString('\n')
	if err != nil {
		t.Fatalf("erstes Teilstueck: %v", err)
	}
	if !strings.Contains(erste, "eins") {
		t.Fatalf("unerwartetes erstes Teilstueck: %q", erste)
	}
	close(losgelassen)

	rest, _ := io.ReadAll(leser)
	if !strings.Contains(string(rest), "[DONE]") {
		t.Errorf("der Strom endete nicht sauber: %q", rest)
	}
}

// OHNE GUELTIGEN SCHLUESSEL WIRD NICHT GEFRAGT — sonst zahlt der Betreiber fuer Anfragen,
// die niemandem gehoeren.
func TestOhneGueltigenSchluesselWirdOpenRouterNichtGefragt(t *testing.T) {
	gefragt := false
	vor, gueltig := bau(t, func(w http.ResponseWriter, r *http.Request) {
		gefragt = true
		_, _ = io.WriteString(w, `{}`)
	})

	for _, fall := range []struct {
		name string
		key  string
	}{
		{"gar keiner", ""},
		{"fremdes Format", "sk-or-v1-ein-openrouter-schluessel"},
		{"richtiges Praefix, erfundene Kennung", schluessel.Praefix + "aaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{"richtige Kennung, falsches Geheimnis", gueltig[:len(gueltig)-4] + "zzzz"},
	} {
		t.Run(fall.name, func(t *testing.T) {
			antw := frage(t, vor, fall.key, `{}`)
			if antw.StatusCode != http.StatusUnauthorized {
				t.Errorf("Status %d statt 401", antw.StatusCode)
			}
			gelesen, _ := io.ReadAll(antw.Body)
			if !strings.Contains(string(gelesen), "kirla_key_ungueltig") {
				t.Errorf("die Fehlerkennung fehlt: %s", gelesen)
			}
			// Die Auskunft geht an einen Menschen und muss den Ausweg nennen.
			if !strings.Contains(string(gelesen), "kirla.ai") {
				t.Errorf("die Meldung sagt nicht, was zu tun ist: %s", gelesen)
			}
		})
	}
	if gefragt {
		t.Fatal("OpenRouter wurde trotz ungueltigem Schluessel gefragt — das kostet Geld")
	}
}

// EIN FEHLER VON OPENROUTER IST NICHT UNSERER. Er geht durch, samt Status und Koerper —
// klabs kennt die Gestalt und behandelt sie seit Langem.
func TestFehlerVonOpenRouterGehtDurch(t *testing.T) {
	const fremd = `{"error":{"message":"upstream ueberlastet","code":429}}`
	vor, key := bau(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, fremd)
	})

	antw := frage(t, vor, key, `{}`)
	if antw.StatusCode != http.StatusTooManyRequests {
		t.Errorf("Status %d statt 429 — der Vorbau hat den Fehler uebersetzt", antw.StatusCode)
	}
	if antw.Header.Get("Retry-After") != "7" {
		t.Errorf("Retry-After ging verloren: %q — klabs wartet dann ins Blaue", antw.Header.Get("Retry-After"))
	}
	gelesen, _ := io.ReadAll(antw.Body)
	if string(gelesen) != fremd {
		t.Errorf("der fremde Fehlerkoerper wurde angefasst: %s", gelesen)
	}
}

// VERBINDUNGSGEBUNDENE KOPFZEILEN duerfen nicht weitergereicht werden. Wer sie mitschickt,
// baut einen Vorbau, der bei keep-alive gelegentlich Antworten vertauscht.
func TestVerbindungsgebundeneKopfzeilenGehenNichtWeiter(t *testing.T) {
	var gesehen http.Header
	vor, key := bau(t, func(w http.ResponseWriter, r *http.Request) {
		gesehen = r.Header.Clone()
		_, _ = io.WriteString(w, `{}`)
	})

	r, _ := http.NewRequest(http.MethodPost, vor.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("X-Geht-Durch", "ja")
	r.Header.Set("Connection", "X-Nur-Diese-Verbindung")
	r.Header.Set("X-Nur-Diese-Verbindung", "nein")
	antw, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer antw.Body.Close()

	if gesehen.Get("X-Geht-Durch") != "ja" {
		t.Error("eine normale Kopfzeile ging verloren")
	}
	if gesehen.Get("X-Nur-Diese-Verbindung") != "" {
		t.Error("eine in Connection genannte Kopfzeile wurde weitergereicht")
	}
	if gesehen.Get("Transfer-Encoding") != "" {
		t.Error("Transfer-Encoding wurde weitergereicht")
	}
}

// GO PACKT SONST STILL AUS. Ohne DisableCompression setzt der Transport selbst
// `Accept-Encoding: gzip` und liefert andere Bytes zurueck, als OpenRouter geschickt hat —
// "unveraendert" waere dann gelogen. Der Test prueft die sichtbare Folge: was der Kunde
// gefragt hat, steht auf der Leitung, und sonst nichts.
func TestDerVorbauErfindetKeineInhaltskodierung(t *testing.T) {
	var gesehen string
	vor, key := bau(t, func(w http.ResponseWriter, r *http.Request) {
		gesehen = r.Header.Get("Accept-Encoding")
		_, _ = io.WriteString(w, `{}`)
	})

	// Der Client von Go setzt seinerseits gzip, wenn man ihn laesst; hier wird der Kopf
	// ausdruecklich gesetzt, damit der Vergleich eindeutig ist.
	r, _ := http.NewRequest(http.MethodPost, vor.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Accept-Encoding", "identity")
	antw, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer antw.Body.Close()

	if gesehen != "identity" {
		t.Errorf("Accept-Encoding kam als %q an — der Vorbau hat es ersetzt", gesehen)
	}
}

// UNSERE EIGENEN PFADE GEHEN NICHT AN OPENROUTER.
//
// /v1/key ist der ernstere Fall und der Grund, warum dieser Test existiert: klabs ruft ihn
// aus `llm-budget`, um den anbieterseitigen Ausgabendeckel zu pruefen. Durchgereicht saehe
// OpenRouter den HAUPTSCHLUESSEL und antwortete mit dem Zustand des BETREIBERKONTOS --
// Verbrauch, Limit und Rest ALLER Kunden zusammen. Der Kunde bekaeme fremde Zahlen zu
// sehen, und klabs hielte das ganze Betreiberkonto fuer sein eigenes Limit.
//
// /v1/route ist die Auskunft ueber die Ablage (Auftrag §5) und gehoert ebenso uns.
func TestUnserePfadeGehenNichtAnOpenRouter(t *testing.T) {
	for _, fall := range []struct {
		pfad   string
		status int
	}{
		// Ohne Toepfe kann /v1/key kein Guthaben ausweisen -- und meldet ausdruecklich
		// NICHT das Betreiberkonto.
		{"/v1/key", http.StatusNotImplemented},
		// /v1/route antwortet immer: die Auskunft haengt an keiner Datenbank.
		{"/v1/route", http.StatusOK},
	} {
		t.Run(fall.pfad, func(t *testing.T) {
			gefragt := false
			vor, key := bau(t, func(w http.ResponseWriter, r *http.Request) {
				gefragt = true
				// Genau die Gestalt, die OpenRouter fuer das Betreiberkonto liefert.
				_, _ = io.WriteString(w, `{"data":{"label":"betreiberkonto","usage":842.17,"limit":2000,"limit_remaining":1157.83}}`)
			})

			r, _ := http.NewRequest(http.MethodGet, vor.URL+fall.pfad, nil)
			r.Header.Set("Authorization", "Bearer "+key)
			antw, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer antw.Body.Close()
			gelesen, _ := io.ReadAll(antw.Body)

			if gefragt {
				t.Fatalf("%s wurde an OpenRouter weitergereicht", fall.pfad)
			}
			if antw.StatusCode != fall.status {
				t.Errorf("Status %d statt %d: %s", antw.StatusCode, fall.status, gelesen)
			}
			// Der Beweis, auf den es ankommt: keine fremde Zahl im Koerper.
			for _, verraeter := range []string{"842.17", "1157.83", "betreiberkonto"} {
				if strings.Contains(string(gelesen), verraeter) {
					t.Fatalf("ZAHLEN DES BETREIBERKONTOS SIND DURCHGEDRUNGEN (%q): %s", verraeter, gelesen)
				}
			}
		})
	}
}

// OHNE SCHLUESSEL GIBT ES AUCH KEINE AUSKUNFT. Was gespeichert wird, geht Kunden etwas an
// und Fremde nichts.
func TestUnserePfadeVerlangenEinenSchluessel(t *testing.T) {
	vor, _ := bau(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, pfad := range []string{"/v1/route", "/v1/key"} {
		r, _ := http.NewRequest(http.MethodGet, vor.URL+pfad, nil)
		antw, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		antw.Body.Close()
		if antw.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s ohne Schluessel: Status %d statt 401", pfad, antw.StatusCode)
		}
	}
}

// DIE AUSKUNFT MUSS DIE UNANGENEHME ZEILE ENTHALTEN (Auftrag §5, klabs ADR-0016).
//
// "Die Geschaeftsinhalte aller Kundenfirmen liegen fuer N Tage bei Kirla. Das gehoert in
// die Nutzungsbedingungen, in `klabs route show` und in die Einrichtung -- nicht in eine
// Fussnote." Der Test haelt fest, dass die Zahl und das Wort wirklich dastehen.
func TestDieRouteAuskunftSagtWasGespeichertWirdUndWieLange(t *testing.T) {
	vor, key := bau(t, func(w http.ResponseWriter, r *http.Request) {})
	r, _ := http.NewRequest(http.MethodGet, vor.URL+"/v1/route", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	antw, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer antw.Body.Close()

	var a struct {
		Kurzfassung string `json:"kurzfassung"`
		Gespeichert struct {
			Inhalte struct {
				Was  string `json:"was"`
				Tage *int   `json:"tage"`
			} `json:"inhalte"`
			Kopfdaten struct {
				Aufbewahrung string `json:"aufbewahrung"`
			} `json:"kopfdaten"`
		} `json:"gespeichert"`
	}
	if err := json.NewDecoder(antw.Body).Decode(&a); err != nil {
		t.Fatalf("die Auskunft ist kein lesbares JSON: %v", err)
	}
	if a.Gespeichert.Inhalte.Tage == nil || *a.Gespeichert.Inhalte.Tage != 30 {
		t.Errorf("die Aufbewahrungsfrist fehlt oder stimmt nicht: %+v", a.Gespeichert.Inhalte.Tage)
	}
	if !strings.Contains(a.Gespeichert.Inhalte.Was, "vollstaendig") {
		t.Errorf("es steht nicht da, dass VOLLSTAENDIG gespeichert wird: %q", a.Gespeichert.Inhalte.Was)
	}
	if a.Gespeichert.Kopfdaten.Aufbewahrung != "dauerhaft" {
		t.Errorf("die Kopfdaten sind nicht als dauerhaft ausgewiesen: %q", a.Gespeichert.Kopfdaten.Aufbewahrung)
	}
	// Die Kurzfassung ist das, was ein Mensch liest. Sie muss die Frist nennen.
	if !strings.Contains(a.Kurzfassung, "30") || !strings.Contains(a.Kurzfassung, "geloescht") {
		t.Errorf("die Kurzfassung nennt die Frist nicht: %q", a.Kurzfassung)
	}
}

// JEDE ANTWORT TRAEGT EINE ANFRAGE-ID, auch die abgelehnte — sonst laesst sich eine
// Rueckfrage keiner Zeile im Protokoll zuordnen (Auftrag §3).
func TestJedeAntwortTraegtEineAnfrageID(t *testing.T) {
	vor, key := bau(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})

	for _, fall := range []struct{ name, key string }{
		{"gelungen", key},
		{"abgelehnt", "kaputt"},
	} {
		t.Run(fall.name, func(t *testing.T) {
			antw := frage(t, vor, fall.key, `{}`)
			if antw.Header.Get("X-Kirla-Anfrage-Id") == "" {
				t.Error("X-Kirla-Anfrage-Id fehlt")
			}
		})
	}
}

// EIN ABGEBROCHENER KUNDE MUSS BIS ZU OPENROUTER DURCHSCHLAGEN. Sonst laeuft dort ein
// bezahlter Modellaufruf weiter, den niemand mehr liest.
func TestAbbruchWirktBisZuOpenRouter(t *testing.T) {
	bemerkt := make(chan struct{})

	vor, key := bau(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		steuerung := http.NewResponseController(w)
		_, _ = io.WriteString(w, "data: los\n\n")
		_ = steuerung.Flush()
		select {
		case <-r.Context().Done():
			close(bemerkt)
		case <-time.After(5 * time.Second):
			t.Error("OpenRouter hat den Abbruch nie bemerkt")
		}
	})

	frist, abbrechen := context.WithCancel(context.Background())
	r, _ := http.NewRequestWithContext(frist, http.MethodPost, vor.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	r.Header.Set("Authorization", "Bearer "+key)
	antw, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	puffer := make([]byte, 4)
	_, _ = antw.Body.Read(puffer)
	abbrechen()
	antw.Body.Close()

	select {
	case <-bemerkt:
	case <-time.After(5 * time.Second):
		t.Fatal("der Abbruch kam bei OpenRouter nicht an")
	}
}
