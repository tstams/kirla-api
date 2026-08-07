package vorbau

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/tstams/kirla-api/internal/ablage"
	"github.com/tstams/kirla-api/internal/geld"
	"github.com/tstams/kirla-api/internal/kosten"
	"github.com/tstams/kirla-api/internal/schluessel"
	"github.com/tstams/kirla-api/internal/topf"
)

// bauMitTopf stellt einen Vorbau mit Abrechnung vor ein gefaelschtes OpenRouter.
func bauMitTopf(t *testing.T, guthaben string, echo http.HandlerFunc) (*httptest.Server, string, *topf.Toepfe, chan ablage.Aufruf) {
	t.Helper()
	fern := httptest.NewServer(echo)
	t.Cleanup(fern.Close)

	klartext, eintrag, err := schluessel.Erzeuge("testfirma")
	if err != nil {
		t.Fatal(err)
	}
	ziel, err := url.Parse(fern.URL + "/v1")
	if err != nil {
		t.Fatal(err)
	}
	betrag, err := geld.AusText(guthaben)
	if err != nil {
		t.Fatal(err)
	}
	toepfe := topf.Neu()
	toepfe.Setze(eintrag.Kennung, betrag)
	buch := make(chan ablage.Aufruf, 16)

	v := &Vorbau{
		Ziel:            ziel,
		Hauptschluessel: func() string { return hauptschluesselTest },
		Verzeichnis:     schluessel.NeuesVerzeichnis([]schluessel.Eintrag{eintrag}),
		Client:          &http.Client{},
		Protokoll:       slog.New(slog.DiscardHandler),
		Toepfe:          toepfe,
		Buch:            buch,
		Aufschlag:       1500,
		Boden:           geld.AusDollar(0.02),
	}
	vor := httptest.NewServer(v)
	t.Cleanup(vor.Close)
	return vor, klartext, toepfe, buch
}

// DER 402 KOMMT, BEVOR OPENROUTER GEFRAGT WIRD (Auftrag §4). Das ist der ganze Sinn des
// Zuruecklegens: ein leerer Topf darf keine Kosten mehr ausloesen.
func TestBeiLeeremTopfWirdOpenRouterNichtGefragt(t *testing.T) {
	gefragt := false
	vor, key, _, buch := bauMitTopf(t, "0", func(w http.ResponseWriter, r *http.Request) {
		gefragt = true
		_, _ = io.WriteString(w, `{"usage":{"cost":1}}`)
	})

	antw := frage(t, vor, key, `{"model":"m","max_tokens":100}`)
	if antw.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("Status %d statt 402", antw.StatusCode)
	}
	if gefragt {
		t.Fatal("OPENROUTER WURDE TROTZ LEEREM TOPF GEFRAGT — das kostet den Betreiber Geld")
	}
	gelesen, _ := io.ReadAll(antw.Body)
	if !strings.Contains(string(gelesen), "kirla_guthaben_leer") {
		t.Errorf("die Fehlerkennung fehlt: %s", gelesen)
	}
	// Die Meldung geht an einen Menschen und muss den Ausweg nennen (Auftrag §3).
	if !strings.Contains(string(gelesen), "kirla.ai") {
		t.Errorf("die Meldung sagt nicht, was zu tun ist: %s", gelesen)
	}
	// Auch die Absage gehoert in die Kopfdaten -- sonst sieht niemand, dass ein Kunde
	// gegen die Wand laeuft.
	select {
	case a := <-buch:
		if a.Status != http.StatusPaymentRequired {
			t.Errorf("gebucht mit Status %d", a.Status)
		}
		if a.Verkauf != 0 {
			t.Errorf("eine abgelehnte Anfrage wurde berechnet: %s", a.Verkauf)
		}
	default:
		t.Error("die Absage wurde nicht gebucht")
	}
}

// ABGERECHNET WIRD MIT DEM, WAS GEMELDET WURDE -- plus dem ausgewiesenen Aufschlag.
func TestAbgerechnetWirdMitDenGemeldetenKosten(t *testing.T) {
	vor, key, toepfe, buch := bauMitTopf(t, "1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"model":"m","usage":{"prompt_tokens":10,"completion_tokens":20,"cost":0.0042}}`)
	})

	antw := frage(t, vor, key, `{"model":"m","max_tokens":20}`)
	io.Copy(io.Discard, antw.Body)
	if antw.StatusCode != http.StatusOK {
		t.Fatalf("Status %d", antw.StatusCode)
	}

	a := <-buch
	if a.Einkauf != geld.Betrag(4_200_000) {
		t.Errorf("Einkauf %s statt 0,0042", a.Einkauf)
	}
	// 0,0042 + 15 % = 0,00483
	if a.Verkauf != geld.Betrag(4_830_000) {
		t.Errorf("Verkauf %s statt 0,00483", a.Verkauf)
	}
	if a.KostenQuelle != kosten.Gemeldet {
		t.Errorf("Quelle %q", a.KostenQuelle)
	}
	if a.TokensEin != 10 || a.TokensAus != 20 {
		t.Errorf("Tokens %d/%d", a.TokensEin, a.TokensAus)
	}

	// Die Ruecklage muss aufgeloest sein, sonst schrumpft der Topf ohne Grund.
	s, _ := toepfe.Stand(a.Kennung)
	if s.Zurueckgelegt != 0 {
		t.Errorf("es blieb etwas zurueckgelegt: %s", s.Zurueckgelegt)
	}
	if s.Verbraucht != geld.Betrag(4_830_000) {
		t.Errorf("verbraucht %s statt 0,00483", s.Verbraucht)
	}

	// Und die Kopfzeilen stehen dran (Auftrag §3) -- ohne Strom geht das.
	if antw.Header.Get("X-Kirla-Kosten-USD") != "0.004830" {
		t.Errorf("X-Kirla-Kosten-USD = %q", antw.Header.Get("X-Kirla-Kosten-USD"))
	}
	// "was im Topf DANACH uebrig ist": 1 - 0,00483.
	if antw.Header.Get("X-Kirla-Guthaben-USD") != "0.995170" {
		t.Errorf("X-Kirla-Guthaben-USD = %q — das ist nicht der Stand DANACH", antw.Header.Get("X-Kirla-Guthaben-USD"))
	}
}

// "KEIN FELD" UND "FELD STEHT AUF NULL" WERDEN AUCH HIER UNTERSCHIEDEN.
//
// Der Auftrag §4 verlangt den Boden, wenn OpenRouter keine Kosten meldet. Ein kostenloses
// Modell meldet aber ausdruecklich 0 -- gemessen an inclusionai/ling-3.0-tiny:free. Wer
// beides gleich behandelt, stellt Gratismodelle mit 2 Cent in Rechnung.
func TestFreiKostetNichtsUndUnbekanntKostetDenBoden(t *testing.T) {
	for _, f := range []struct {
		name    string
		antwort string
		verkauf geld.Betrag
		quelle  string
	}{
		{"kostenloses Modell", `{"usage":{"cost":0}}`, 0, kosten.Frei},
		// Boden 0,02 + 15 % = 0,023
		{"keine Kostenangabe", `{"usage":{"prompt_tokens":1}}`, geld.Betrag(23_000_000), kosten.Boden},
	} {
		t.Run(f.name, func(t *testing.T) {
			vor, key, _, buch := bauMitTopf(t, "1", func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, f.antwort)
			})
			antw := frage(t, vor, key, `{"model":"m"}`)
			io.Copy(io.Discard, antw.Body)

			a := <-buch
			if a.KostenQuelle != f.quelle {
				t.Errorf("Quelle %q statt %q", a.KostenQuelle, f.quelle)
			}
			if a.Verkauf != f.verkauf {
				t.Errorf("Verkauf %s statt %s", a.Verkauf, f.verkauf)
			}
		})
	}
}

// EIN FEHLER VON OPENROUTER KOSTET NICHTS. Ihm einen Boden aufzuerlegen hiesse, Fehler in
// Rechnung zu stellen.
func TestEinFehlerVonOpenRouterKostetNichts(t *testing.T) {
	vor, key, _, buch := bauMitTopf(t, "1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"message":"kaputt"}}`)
	})

	antw := frage(t, vor, key, `{"model":"m"}`)
	io.Copy(io.Discard, antw.Body)
	if antw.StatusCode != http.StatusBadGateway {
		t.Errorf("Status %d — der Fehler wurde uebersetzt", antw.StatusCode)
	}
	a := <-buch
	if a.Verkauf != 0 {
		t.Errorf("ein Fehler wurde mit %s berechnet", a.Verkauf)
	}
}

// BEI EINEM STROM GIBT ES DIE KOSTENKOPFZEILEN NICHT -- und das ist entschieden, nicht
// vergessen (docs/adr/0002). Gebucht wird trotzdem, aus dem letzten Teilstueck.
func TestBeimStromWirdGebuchtAberNichtImKopfGemeldet(t *testing.T) {
	vor, key, _, buch := bauMitTopf(t, "1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		steuerung := http.NewResponseController(w)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"eins\"}}]}\n\n")
		_ = steuerung.Flush()
		_, _ = io.WriteString(w, "data: {\"model\":\"m\",\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":7,\"cost\":0.001}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		_ = steuerung.Flush()
	})

	antw := frage(t, vor, key, `{"model":"m","stream":true}`)
	gelesen, _ := io.ReadAll(antw.Body)
	if !strings.Contains(string(gelesen), "[DONE]") {
		t.Fatalf("der Strom kam nicht an: %q", gelesen)
	}
	if antw.Header.Get("X-Kirla-Kosten-USD") != "" {
		t.Error("beim Strom steht eine Kostenkopfzeile im Kopf — sie kann dort nicht stimmen")
	}

	a := <-buch
	if !a.Stroemend {
		t.Error("der Aufruf wurde nicht als stroemend gebucht")
	}
	// 0,001 + 15 % = 0,00115
	if a.Verkauf != geld.Betrag(1_150_000) {
		t.Errorf("Verkauf %s statt 0,00115 — die Kosten aus dem letzten Teilstueck fehlen", a.Verkauf)
	}
}

// DER ANFRAGEKOERPER GEHT VOLLSTAENDIG DURCH, obwohl der Anfang zum Spaehen gelesen wurde.
// Ohne das fehlte OpenRouter genau der Teil, den wir angesehen haben.
func TestDerGespaehteKoerperKommtVollstaendigAn(t *testing.T) {
	gross := `{"model":"m","max_tokens":50,"messages":[{"role":"user","content":"` +
		strings.Repeat("abcdefghij", 12_000) + `"}]}`

	var angekommen string
	vor, key, _, _ := bauMitTopf(t, "1", func(w http.ResponseWriter, r *http.Request) {
		roh, _ := io.ReadAll(r.Body)
		angekommen = string(roh)
		_, _ = io.WriteString(w, `{"usage":{"cost":0.001}}`)
	})

	antw := frage(t, vor, key, gross)
	io.Copy(io.Discard, antw.Body)

	if angekommen != gross {
		t.Errorf("der Koerper kam veraendert an: %d statt %d Bytes", len(angekommen), len(gross))
	}
}

// TestMain senkt die bcrypt-Kosten. Ohne das braucht diese Liste unter -race Minuten:
// jeder Test erzeugt und prueft einen Schluessel, und das sind mit Kostenfaktor 12 je
// eine halbe Sekunde. Im Betrieb gilt der Wert aus dem Quelltext.
func TestMain(m *testing.M) {
	schluessel.Kostenfaktor = bcrypt.MinCost
	os.Exit(m.Run())
}
