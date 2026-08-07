package kosten

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tstams/kirla-api/internal/geld"
)

// DER MITLESER DARF DIE BYTES NICHT ANFASSEN. Das ist die Zusage aus §1, und sie faellt
// genau hier, wenn jemand den Puffer statt der Quelle weiterschreibt.
func TestDerMitleserVeraendertNichts(t *testing.T) {
	const antwort = `{"model":"m","choices":[{"message":{"tool_calls":[
	  {"id":"c1","function":{"name":"fs_write",
	   "arguments":"{\"path\": \"/x.py\", \"content\": \"\\\"\\\"\\\"\\nabgeschnitten"}}]}}],
	  "usage":{"prompt_tokens":10,"completion_tokens":2048,"cost":0.002}}`

	var raus bytes.Buffer
	m := NeuerMitleser(&raus, 1024)
	// Stueckweise schreiben, wie ein Strom es taete.
	for i := 0; i < len(antwort); i += 7 {
		ende := min(i+7, len(antwort))
		if _, err := m.Write([]byte(antwort[i:ende])); err != nil {
			t.Fatal(err)
		}
	}
	if raus.String() != antwort {
		t.Errorf("durchgereicht wurde etwas anderes:\n%s", raus.String())
	}
	if m.Gesamt != int64(len(antwort)) {
		t.Errorf("Gesamt=%d statt %d", m.Gesamt, len(antwort))
	}
}

// DER SCHWANZ IST BEGRENZT. Ohne Grenze haengt der Speicher an der Laenge der Antwort --
// genau die Eigenschaft, die der Vorbau nicht haben darf.
func TestDerSchwanzWaechstNichtUnbegrenzt(t *testing.T) {
	var raus bytes.Buffer
	m := NeuerMitleser(&raus, 100)
	gross := strings.Repeat("x", 10_000)
	if _, err := m.Write([]byte(gross)); err != nil {
		t.Fatal(err)
	}
	if len(m.Schwanz()) != 100 {
		t.Errorf("Schwanz ist %d Bytes lang statt 100", len(m.Schwanz()))
	}
	if raus.Len() != 10_000 {
		t.Errorf("durchgereicht wurden %d Bytes statt 10000", raus.Len())
	}
}

// EINE GEWOEHNLICHE ANTWORT.
func TestKostenAusEinerGanzenAntwort(t *testing.T) {
	const a = `{"model":"anbieter/modell","choices":[],"usage":{"prompt_tokens":246,"completion_tokens":48,"cost":0.0042}}`
	v := Lies([]byte(a), false)
	if v.Quelle != Gemeldet {
		t.Errorf("Quelle %q statt gemeldet", v.Quelle)
	}
	if v.Einkauf != geld.Betrag(4_200_000) {
		t.Errorf("Einkauf %s statt 0,0042", v.Einkauf)
	}
	if v.TokensEin != 246 || v.TokensAus != 48 {
		t.Errorf("Tokens %d/%d", v.TokensEin, v.TokensAus)
	}
	if v.Modell != "anbieter/modell" {
		t.Errorf("Modell %q", v.Modell)
	}
}

// NEUN NACHKOMMASTELLEN UEBERLEBEN. Die Zahl stammt aus der echten Antwort vom 07.08.2026.
func TestNeunStellenUeberlebenAuchHier(t *testing.T) {
	v := Lies([]byte(`{"usage":{"cost":0.016653351}}`), false)
	if v.Einkauf != geld.Betrag(16_653_351) {
		t.Errorf("Einkauf %d Nano statt 16653351", int64(v.Einkauf))
	}
}

// AUS EINEM STROM. usage steht im LETZTEN Teilstueck vor [DONE]; frueher gefundene
// Stuecke sind Zwischenstaende und wuerden zu niedrig buchen.
func TestKostenAusEinemStrom(t *testing.T) {
	const strom = `data: {"model":"m","choices":[{"delta":{"content":"eins"}}]}

data: {"model":"m","choices":[{"delta":{"content":"zwei"}}]}

data: {"model":"m","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":250,"cost":0.00789}}

data: [DONE]

`
	v := Lies([]byte(strom), true)
	if v.Quelle != Gemeldet {
		t.Fatalf("Quelle %q statt gemeldet", v.Quelle)
	}
	if v.Einkauf != geld.Betrag(7_890_000) {
		t.Errorf("Einkauf %s statt 0,00789", v.Einkauf)
	}
	if v.TokensAus != 250 {
		t.Errorf("TokensAus %d statt 250", v.TokensAus)
	}
}

// "KEIN FELD" UND "FELD STEHT AUF NULL" SIND NICHT DASSELBE.
//
// Der Auftrag §4 verlangt einen Boden, wenn OpenRouter keine Kosten meldet. Ein
// kostenloses Modell meldet aber ausdruecklich cost=0 -- gemessen am 07.08.2026 an
// inclusionai/ling-3.0-tiny:free. Wer beides gleich behandelt, stellt Gratismodelle in
// Rechnung.
func TestFreiUndUnbekanntWerdenUnterschieden(t *testing.T) {
	for _, f := range []struct {
		name   string
		roh    string
		quelle string
	}{
		{"kostenloses Modell meldet 0", `{"usage":{"prompt_tokens":246,"completion_tokens":48,"cost":0}}`, Frei},
		{"Feld fehlt ganz", `{"usage":{"prompt_tokens":246,"completion_tokens":48}}`, Boden},
		{"Feld ist null", `{"usage":{"cost":null}}`, Boden},
		{"gar kein usage", `{"model":"m","choices":[]}`, Boden},
		{"kein lesbares JSON", `{"model":"m","choi`, Boden},
	} {
		t.Run(f.name, func(t *testing.T) {
			v := Lies([]byte(f.roh), false)
			if v.Quelle != f.quelle {
				t.Errorf("Quelle %q statt %q", v.Quelle, f.quelle)
			}
			if f.quelle != Gemeldet && v.Einkauf != 0 {
				t.Errorf("Einkauf %s statt 0", v.Einkauf)
			}
		})
	}
}

// WENN DER SCHWANZ MITTEN IN DIE ANTWORT SCHNEIDET, wird der Boden gebucht und nicht null.
// Zu wenig zu buchen waere der teure Fehler.
func TestEinAbgeschnittenerSchwanzBuchtDenBoden(t *testing.T) {
	v := Lies([]byte(`ten":48,"cost":0.0042}}`), false)
	if v.Quelle != Boden {
		t.Errorf("Quelle %q statt boden", v.Quelle)
	}
}

func TestIstStrom(t *testing.T) {
	for _, f := range []struct {
		typ string
		ja  bool
	}{
		{"text/event-stream", true},
		{"text/event-stream; charset=utf-8", true},
		{"TEXT/EVENT-STREAM", true},
		{"application/json", false},
		{"", false},
	} {
		if got := IstStrom(f.typ); got != f.ja {
			t.Errorf("IstStrom(%q) = %v", f.typ, got)
		}
	}
}
