package vorbau

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tstams/kirla-api/internal/ablage"
	"github.com/tstams/kirla-api/internal/geld"
	"github.com/tstams/kirla-api/internal/kulanz"
)

// bauMitLage haengt eine Kulanz-Lage an einen Vorbau mit LEEREM Topf. Der leere Topf ist
// der Punkt: er trennt "gesund" (402) von "im Fenster" (durchreichen).
func bauMitLage(t *testing.T, echo http.HandlerFunc) (*httptest.Server, string, chan ablage.Aufruf, *kulanz.Lage) {
	t.Helper()
	vor, key, _, buch := bauMitTopf(t, "0", echo)
	v := vor.Config.Handler.(*Vorbau)
	lage := kulanz.Neu(time.Hour)
	v.Lage = lage
	return vor, key, buch, lage
}

// IM KULANZFENSTER WIRD NICHT ZURUECKGELEGT -- und damit auch kein 402 geschickt.
//
// Auftrag §4. Der Grund steht in §0: die Toepfe im Speicher sind bei ausgefallener
// Datenbank veraltet. Einer Kundenfirma auf einen VERALTETEN Stand hin den Modellzugang zu
// nehmen, waere genau der Single Point of Failure, den ADR-0016 ausschliesst -- der Ausfall
// ist unserer, nicht ihrer.
func TestImKulanzfensterGibtEsKeinen402(t *testing.T) {
	gefragt := 0
	vor, key, buch, lage := bauMitLage(t, func(w http.ResponseWriter, r *http.Request) {
		gefragt++
		_, _ = io.WriteString(w, `{"model":"m","usage":{"cost":0.001}}`)
	})

	// Erst der gesunde Fall: leerer Topf -> 402, und OpenRouter wird nicht gefragt.
	lage.Gelungen()
	antw := frage(t, vor, key, `{"model":"m"}`)
	io.Copy(io.Discard, antw.Body)
	if antw.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("gesund: Status %d statt 402", antw.StatusCode)
	}
	if gefragt != 0 {
		t.Fatal("gesund: OpenRouter wurde trotz leerem Topf gefragt")
	}
	<-buch // die Absage wurde gebucht

	// Jetzt faellt die Datenbank aus. Derselbe leere Topf -- die Anfrage geht durch.
	lage.Gescheitert()
	antw = frage(t, vor, key, `{"model":"m"}`)
	io.Copy(io.Discard, antw.Body)
	if antw.StatusCode != http.StatusOK {
		t.Fatalf("im Fenster: Status %d statt 200 — dem Kunden wurde der Zugang genommen, weil UNSERE Datenbank weg ist", antw.StatusCode)
	}
	if gefragt != 1 {
		t.Fatal("im Fenster: die Anfrage wurde nicht weitergereicht")
	}

	// GEBUCHT WIRD TROTZDEM -- nur eben nicht zurueckgelegt. Ohne das waere jeder
	// Datenbankausfall ein Freibrief.
	a := <-buch
	if a.Verkauf != geld.Betrag(1_150_000) { // 0,001 + 15 %
		t.Errorf("im Fenster wurde %s gebucht statt 0,00115", a.Verkauf)
	}
}

// NACH DEM FENSTER IST SCHLUSS. Das ist der bewusst gezahlte Preis aus ADR-0016 und die
// Grenze der Zusage -- nicht "unbegrenzt weiter, solange es keiner merkt".
func TestNachDemFensterWirdNichtsMehrWeitergereicht(t *testing.T) {
	gefragt := 0
	vor, key, _, lage := bauMitLage(t, func(w http.ResponseWriter, r *http.Request) {
		gefragt++
	})

	// Ohne einen einzigen Erfolg gilt die Lage als abgelaufen: ein Dienst, dessen
	// Datenbank nie geantwortet hat, kennt weder Schluessel noch Toepfe.
	lage.Gescheitert()
	antw := frage(t, vor, key, `{"model":"m"}`)
	gelesen, _ := io.ReadAll(antw.Body)

	if antw.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("Status %d statt 503", antw.StatusCode)
	}
	if gefragt != 0 {
		t.Fatal("nach dem Fenster wurde OpenRouter noch gefragt")
	}
	if !strings.Contains(string(gelesen), "kirla_kulanz_abgelaufen") {
		t.Errorf("die Fehlerkennung fehlt: %s", gelesen)
	}
	// Die Meldung geht an einen Menschen und muss sagen, woran es liegt.
	if !strings.Contains(string(gelesen), "24 Stunden") {
		t.Errorf("die Meldung nennt die Frist nicht: %s", gelesen)
	}
}

// OHNE LAGE VERHAELT SICH ALLES WIE VORHER. Der Vorbau muss ohne Kulanzfenster laufen
// koennen -- die Durchreich-Tests aus Schritt 1 setzen keines.
func TestOhneLageBleibtAllesWieVorher(t *testing.T) {
	vor, key, _, _ := bauMitTopf(t, "0", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	if v := vor.Config.Handler.(*Vorbau); v.Lage != nil {
		t.Fatal("da haengt eine Lage, wo keine sein sollte")
	}
	antw := frage(t, vor, key, `{"model":"m"}`)
	io.Copy(io.Discard, antw.Body)
	if antw.StatusCode != http.StatusPaymentRequired {
		t.Errorf("Status %d statt 402", antw.StatusCode)
	}
}
