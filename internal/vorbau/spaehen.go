package vorbau

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strconv"
)

// Wie viel vom Anfragekoerper hoechstens gelesen wird, um Modell und max_tokens zu finden.
//
// DER KOERPER DARF NICHT GEPUFFERT WERDEN (Auftrag §3) -- eine Anfrage mit langem Verlauf
// kann Megabytes haben, und der Vorbau soll seinen Speicher nicht an ihre Laenge haengen.
// Gespaeht wird deshalb nur der Anfang; was gelesen wurde, wird davorgesetzt und der Rest
// laeuft weiter durch. 64 KB reichen fuer die Kopffelder mit grossem Abstand.
const spaehGrenze = 64 * 1024

// gespaeht ist, was sich aus dem Anfang der Anfrage ablesen liess.
type gespaeht struct {
	Modell       string
	MaxAusgabe   int
	Stroemend    bool
	Vollstaendig bool // der Koerper passte ganz in die Spaehgrenze
}

// Diese Ausdruecke greifen, wenn der Koerper laenger ist als die Spaehgrenze und das JSON
// deshalb abgeschnitten bei uns ankommt. Sie sind absichtlich anspruchslos: sie muessen
// nur ein Feld finden, nicht JSON verstehen.
var (
	suchModell     = regexp.MustCompile(`"model"\s*:\s*"([^"]{1,200})"`)
	suchMaxTokens  = regexp.MustCompile(`"max_tokens"\s*:\s*(\d{1,9})`)
	suchMaxAusgabe = regexp.MustCompile(`"max_completion_tokens"\s*:\s*(\d{1,9})`)
	suchStrom      = regexp.MustCompile(`"stream"\s*:\s*true`)
)

// spaehe liest den Anfang des Koerpers, liest Modell und Grenzen heraus und gibt einen
// Leser zurueck, der wieder ALLES liefert -- den gelesenen Anfang und den Rest.
func spaehe(koerper io.ReadCloser) (gespaeht, io.Reader) {
	if koerper == nil {
		return gespaeht{}, nil
	}
	anfang, err := io.ReadAll(io.LimitReader(koerper, spaehGrenze))
	if err != nil {
		// Was gelesen wurde, geht trotzdem weiter; der Fehler faellt beim Weiterreichen
		// erneut auf und wird dort behandelt.
		return gespaeht{}, io.MultiReader(bytes.NewReader(anfang), koerper)
	}
	zusammen := io.Reader(io.MultiReader(bytes.NewReader(anfang), koerper))

	g := gespaeht{}
	// Erst der saubere Weg: passt der ganze Koerper in die Spaehgrenze, ist er gueltiges
	// JSON und wird richtig gelesen.
	var ganz struct {
		Model               string `json:"model"`
		MaxTokens           *int   `json:"max_tokens"`
		MaxCompletionTokens *int   `json:"max_completion_tokens"`
		Stream              bool   `json:"stream"`
	}
	if len(anfang) < spaehGrenze && json.Unmarshal(bytes.TrimSpace(anfang), &ganz) == nil {
		g.Modell = ganz.Model
		g.Stroemend = ganz.Stream
		g.Vollstaendig = true
		switch {
		case ganz.MaxTokens != nil:
			g.MaxAusgabe = *ganz.MaxTokens
		case ganz.MaxCompletionTokens != nil:
			g.MaxAusgabe = *ganz.MaxCompletionTokens
		}
		return g, zusammen
	}

	// Sonst das Grobe: im Anfang nach den Feldern suchen.
	if m := suchModell.FindSubmatch(anfang); m != nil {
		g.Modell = string(m[1])
	}
	if m := suchMaxTokens.FindSubmatch(anfang); m != nil {
		g.MaxAusgabe, _ = strconv.Atoi(string(m[1]))
	} else if m := suchMaxAusgabe.FindSubmatch(anfang); m != nil {
		g.MaxAusgabe, _ = strconv.Atoi(string(m[1]))
	}
	g.Stroemend = suchStrom.Match(anfang)
	return g, zusammen
}
