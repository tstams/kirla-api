package vorbau

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/tstams/kirla-api/internal/ablage"
	"github.com/tstams/kirla-api/internal/schluessel"
)

// ── GET /v1/route: WAS WIRD GESPEICHERT, UND WIE LANGE ─────────────────────────────────
//
// Auftrag §5: "Und es muss nachlesbar sein. `klabs route show` soll auf der Kundenseite
// sagen, was gespeichert wird und wie lange — dafuer braucht es einen Endpunkt
// GET /v1/route mit genau dieser Auskunft. Eine Zwischenstelle, deren Umfang man nicht
// nachlesen kann, ist eine Wette."
//
// klabs ADR-0016 sagt dasselbe von der anderen Seite: "Die Geschaeftsinhalte aller
// Kundenfirmen liegen fuer N Tage bei Kirla. Das gehoert in die Nutzungsbedingungen, in
// `klabs route show` und in die Einrichtung -- nicht in eine Fussnote."
//
// Deshalb steht hier die unangenehme Zeile zuerst und nicht am Ende.

type routeAuskunft struct {
	Dienst      string           `json:"dienst"`
	Kurzfassung string           `json:"kurzfassung"`
	Gespeichert routeGespeichert `json:"gespeichert"`
	Abrechnung  routeAbrechnung  `json:"abrechnung"`
	Verfuegbar  routeVerfuegbar  `json:"verfuegbarkeit"`
	Bestand     *routeBestand    `json:"bestand,omitempty"`
}

type routeGespeichert struct {
	Inhalte   routeTeil `json:"inhalte"`
	Kopfdaten routeTeil `json:"kopfdaten"`
}

type routeTeil struct {
	Was          string `json:"was"`
	Aufbewahrung string `json:"aufbewahrung"`
	Tage         *int   `json:"tage,omitempty"`
	Hinweis      string `json:"hinweis,omitempty"`
}

type routeAbrechnung struct {
	Waehrung         string  `json:"waehrung"`
	AufschlagProzent float64 `json:"aufschlag_prozent"`
	BodenUSD         string  `json:"boden_usd"`
	Hinweis          string  `json:"hinweis"`
}

type routeVerfuegbar struct {
	KulanzfensterStunden float64 `json:"kulanzfenster_stunden"`
	Zustand              string  `json:"zustand"`
	Hinweis              string  `json:"hinweis"`
}

type routeBestand struct {
	Saetze    int64  `json:"saetze"`
	Bytes     int64  `json:"bytes"`
	Aeltester string `json:"aeltester,omitempty"`
	Gemessen  string `json:"gemessen"`
}

// routeAuskunft beantwortet GET /v1/route.
func (v *Vorbau) routeAuskunft(w http.ResponseWriter, e schluessel.Eintrag, anfrageID string) {
	tage := v.aufbewahrungTage()

	a := routeAuskunft{
		Dienst: "kirla.ai",
		Kurzfassung: "Jede Anfrage und jede Antwort werden bei kirla.ai gespeichert und nach " +
			itoa(tage) + " Tagen geloescht. Die Abrechnungsdaten daneben bleiben dauerhaft.",
		Gespeichert: routeGespeichert{
			Inhalte: routeTeil{
				Was:          "Anfrage und Antwort vollstaendig, komprimiert",
				Aufbewahrung: itoa(tage) + " Tage, danach geloescht",
				Tage:         &tage,
				Hinweis: "Darin stehen Ihre Systemtexte, Ihr Gespraechsverlauf und die Antworten des Modells. " +
					"Sehr grosse Koerper werden bei " + itoa(v.grenze()/1024) + " KB je Richtung gekuerzt; das steht dann am Satz.",
			},
			Kopfdaten: routeTeil{
				Was:          "Zeitpunkt, Schluesselkennung, Modell, Tokenzahlen, Kosten, Dauer, Statuscode",
				Aufbewahrung: "dauerhaft",
				Hinweis:      "Das ist die Abrechnung und die Zeitreihe. Ohne sie liesse sich keine Rechnung erklaeren.",
			},
		},
		Abrechnung: routeAbrechnung{
			Waehrung:         "USD",
			AufschlagProzent: float64(v.Aufschlag) / 100,
			BodenUSD:         v.Boden.Text(2),
			Hinweis: "Berechnet wird, was der Modellanbieter meldet, plus Aufschlag. Meldet er keine Kosten, " +
				"wird der Boden gebucht; meldet er ausdruecklich null (kostenloses Modell), wird nichts berechnet.",
		},
		Verfuegbar: routeVerfuegbar{
			KulanzfensterStunden: v.kulanzStunden(),
			Zustand:              v.zustandsname(),
			Hinweis: "Faellt die Abrechnung von kirla.ai aus, laufen Ihre Anfragen weiter -- bis zu " +
				itoa(int(v.kulanzStunden())) + " Stunden. Danach ruht der Zugang.",
		},
	}
	if v.Bestand != nil {
		if b := v.Bestand(); !b.Gemessen.IsZero() {
			r := routeBestand{Saetze: b.Saetze, Bytes: b.Bytes, Gemessen: b.Gemessen.UTC().Format(time.RFC3339)}
			if !b.Aeltester.IsZero() {
				r.Aeltester = b.Aeltester.UTC().Format(time.RFC3339)
			}
			a.Bestand = &r
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(a)
}

func (v *Vorbau) aufbewahrungTage() int {
	if v.AufbewahrungTage > 0 {
		return v.AufbewahrungTage
	}
	return 30
}

func (v *Vorbau) kulanzStunden() float64 {
	if v.Lage == nil {
		return 0
	}
	return v.Lage.Fenster().Hours()
}

func (v *Vorbau) zustandsname() string {
	if v.Lage == nil {
		return "gesund"
	}
	z, _ := v.Lage.Zustand()
	return z.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	minus := n < 0
	if minus {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if minus {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// Bestandsmessung ist der Stand der Ablage, im Hintergrund gemessen.
type Bestandsmessung struct {
	ablage.Ablagestand
	Gemessen time.Time
}
