// kirla-api ist die Zwischenstelle zwischen einer klabs-Instanz und OpenRouter.
//
// Aufruf:
//
//	kirla-api dienst                      den Vorbau starten
//	kirla-api schluessel neu <name>       einen Kundenschluessel vergeben (zeigt ihn EINMAL)
//	kirla-api schluessel liste            vergebene Schluessel und Toepfe zeigen
//	kirla-api schluessel sperren <kennung>
//	kirla-api schluessel oeffnen <kennung>
//	kirla-api schluessel uebernehmen      Schluessel aus der alten Datei in die Datenbank holen
//	kirla-api topf aufladen <kennung> <USD>
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tstams/kirla-api/internal/ablage"
	"github.com/tstams/kirla-api/internal/geld"
	"github.com/tstams/kirla-api/internal/kulanz"
	"github.com/tstams/kirla-api/internal/nachbuch"
	"github.com/tstams/kirla-api/internal/preise"
	"github.com/tstams/kirla-api/internal/schluessel"
	"github.com/tstams/kirla-api/internal/topf"
	"github.com/tstams/kirla-api/internal/vorbau"
)

// Aufschlag in Hundertstel Prozent. Der Auftrag §4 nennt 15 %.
const aufschlag int64 = 1500

// Boden, wenn OpenRouter keine Kosten meldet (Auftrag §4: klabs nennt ihn UnbekanntUSD,
// 2 Cent). Er gilt NUR fuer "kein Kostenfeld" -- ein Modell, das ausdruecklich 0 meldet,
// ist frei und bleibt frei.
var boden = geld.AusDollar(0.02)

func main() {
	protokoll := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "aufruf: kirla-api dienst | schluessel … | topf aufladen <kennung> <USD>")
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "dienst":
		err = dienst(protokoll)
	case "schluessel":
		err = schluesselVerwalten(os.Args[2:])
	case "topf":
		err = topfVerwalten(os.Args[2:])
	default:
		err = fmt.Errorf("unbekannter befehl %q", os.Args[1])
	}
	if err != nil {
		protokoll.Error("abbruch", "fehler", err)
		os.Exit(1)
	}
}

func umgebung(name, vorgabe string) string {
	if wert := os.Getenv(name); wert != "" {
		return wert
	}
	return vorgabe
}

const (
	vorgabeSchluesselDatei = "/srv/kirla-labs/dev/secrets/kirla-schluessel.json"
	vorgabeHauptschluessel = "/srv/kirla-labs/dev/secrets/openrouter.env"
)

// kulanzFenster liest die Frist aus der Umgebung. Vorgabe sind die 24 Stunden aus
// ADR-0016; einstellbar bleibt sie fuer den Betrieb und fuer die Probe.
func kulanzFenster() time.Duration {
	roh := os.Getenv("KIRLA_API_KULANZ")
	if roh == "" {
		return kulanz.VorgabeFenster
	}
	d, err := time.ParseDuration(roh)
	if err != nil || d <= 0 {
		return kulanz.VorgabeFenster
	}
	return d
}

func datenbankURL() (string, error) {
	if u := os.Getenv("KIRLA_API_DATENBANK"); u != "" {
		return u, nil
	}
	// Dieselbe Datei, aus der die uebrigen Labs-Dienste ihre Verbindung nehmen.
	roh, err := os.ReadFile(umgebung("KIRLA_API_DATENBANK_DATEI", "/srv/kirla-labs/shared/.env"))
	if err != nil {
		return "", fmt.Errorf("DATABASE_URL: %w", err)
	}
	for _, z := range strings.Split(string(roh), "\n") {
		if wert, gefunden := strings.CutPrefix(strings.TrimSpace(z), "DATABASE_URL="); gefunden {
			return strings.Trim(strings.TrimSpace(wert), `"'`), nil
		}
	}
	return "", errors.New("in der Umgebungsdatei steht kein DATABASE_URL")
}

func oeffneAblage(ctx context.Context) (*ablage.Ablage, error) {
	url, err := datenbankURL()
	if err != nil {
		return nil, err
	}
	a, err := ablage.Oeffne(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := a.Schema(ctx); err != nil {
		a.Schliesse()
		return nil, err
	}
	return a, nil
}

func dienst(protokoll *slog.Logger) error {
	ctx, beenden := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer beenden()

	adresse := umgebung("KIRLA_API_ADRESSE", "127.0.0.1:3303")
	zielRoh := umgebung("KIRLA_API_ZIEL", "https://openrouter.ai/api/v1")
	hauptDatei := umgebung("KIRLA_API_HAUPTSCHLUESSEL", vorgabeHauptschluessel)

	ziel, err := url.Parse(zielRoh)
	if err != nil {
		return fmt.Errorf("KIRLA_API_ZIEL: %w", err)
	}

	// Der Hauptschluessel steht im Arbeitsspeicher und wird NICHT bei jeder Anfrage von
	// der Platte gelesen. Ein Lesevorgang im Anfrageweg heisst: haengt die Platte, haengt
	// der Dienst — genau das, was der Auftrag §0 ausschliesst. Ein SIGHUP laedt neu.
	var hauptschluessel atomic.Pointer[string]
	ladeHaupt := func() error {
		wert, err := leseHauptschluessel(hauptDatei)
		if err != nil {
			return err
		}
		hauptschluessel.Store(&wert)
		return nil
	}
	if err := ladeHaupt(); err != nil {
		return err
	}

	abl, err := oeffneAblage(ctx)
	if err != nil {
		return err
	}
	defer abl.Schliesse()

	// ── DAS KULANZFENSTER (Schritt 3) ────────────────────────────────────────────────
	//
	// Faellt die Datenbank aus, altert der Stand im Arbeitsspeicher und der Vorbau
	// antwortet weiter -- bis zu 24 Stunden (ADR-0016). Was in dieser Zeit an Abrechnung
	// anfaellt, geht in eine Datei und wird nach der Wiederkehr nachgetragen.
	lage := kulanz.Neu(kulanzFenster())
	buchDatei := umgebung("KIRLA_API_NACHBUCH", "/srv/kirla-labs/nachbuch/offen.jsonl")
	nb, err := nachbuch.Neu(buchDatei)
	if err != nil {
		return err
	}
	if wartend, err := nb.Anzahl(); err != nil {
		protokoll.Warn("nachbuch nicht lesbar", "pfad", nb.Pfad(), "fehler", err)
	} else if wartend > 0 {
		protokoll.Warn("es liegt unbearbeitete abrechnung aus einem frueheren ausfall",
			"saetze", wartend, "pfad", nb.Pfad())
	}

	toepfe := topf.Neu()
	verzeichnis := &atomic.Pointer[schluessel.Verzeichnis]{}
	verzeichnis.Store(schluessel.NeuesVerzeichnis(nil))

	uebernimm := func(liste []ablage.SchluesselMitTopf) {
		eintraege := make([]schluessel.Eintrag, 0, len(liste))
		for _, s := range liste {
			eintraege = append(eintraege, s.Eintrag)
		}
		verzeichnis.Store(schluessel.NeuesVerzeichnis(eintraege))
	}

	nachtragen := func(c context.Context) {
		getragen, err := nb.Arbeite(c, abl, 200)
		if err != nil {
			// Waehrend eines Ausfalls faellt das bei JEDEM Takt an -- der Abgleicher
			// daempft seine eigene Meldung, und diese hier gehoert dazu. Warn statt
			// Error: die Datei bleibt liegen, es geht nichts verloren.
			protokoll.Warn("nachbuch noch nicht abgearbeitet — die datei bleibt liegen",
				"getragen", getragen, "pfad", nb.Pfad())
			return
		}
		if getragen > 0 {
			protokoll.Info("nachbuch abgearbeitet", "saetze", getragen)
		}
	}
	abgleicher := &ablage.Abgleicher{
		Ablage: abl, Toepfe: toepfe, Protokoll: protokoll, Lage: lage,
		Takt: 30 * time.Second, Uebernimm: uebernimm, NachDerWiederkehr: nachtragen,
	}
	// Der erste Abgleich MUSS gelingen: ohne Schluessel im Speicher wuerde der Dienst
	// jeden Kunden abweisen, und das saehe aus wie ein Fehler beim Kunden.
	if err := abl.Erreichbar(ctx); err != nil {
		return fmt.Errorf("datenbank beim start nicht erreichbar: %w", err)
	}
	abgleicher.Einmal(ctx)
	go abgleicher.Laufe(ctx)

	client := neuerClient()

	tafel := preise.Neu()
	if err := tafel.Hole(ctx, client, zielRoh, *hauptschluessel.Load()); err != nil {
		// Ohne Preise wird die Schaetzung zum Boden -- der Dienst laeuft, er legt nur
		// grober zurueck.
		protokoll.Warn("preistafel nicht geholt — es wird mit dem Boden geschaetzt", "fehler", err)
	}
	go holePreiseRegelmaessig(ctx, tafel, client, zielRoh, &hauptschluessel, protokoll)

	// Der Kanal ist grosszuegig: bei 0,14 Anfragen/s (Auftrag §6) sind 4096 Saetze mehr
	// als eine Stunde Vorrat, falls die Datenbank haengt.
	buch := make(chan ablage.Aufruf, 4096)
	schreiber := &ablage.Schreiber{
		Ablage: abl, Toepfe: toepfe, Eingang: buch, Protokoll: protokoll, Lage: lage,
		Notfall: nb.Schreibe,
		Stapel:  64, Takt: 2 * time.Second,
	}
	fertig := make(chan struct{})
	go func() { schreiber.Laufe(ctx); close(fertig) }()

	v := &vorbau.Vorbau{
		Ziel:            ziel,
		Hauptschluessel: func() string { return *hauptschluessel.Load() },
		Client:          client,
		Protokoll:       protokoll,
		Toepfe:          toepfe,
		Preise:          tafel,
		Buch:            buch,
		Aufschlag:       aufschlag,
		Boden:           boden,
		Lage:            lage,
	}
	// Das Verzeichnis wird vom Abgleicher ausgetauscht; der Vorbau liest es je Anfrage.
	v.Verzeichnis = verzeichnis.Load()
	go func() {
		uhr := time.NewTicker(5 * time.Second)
		defer uhr.Stop()
		for {
			select {
			case <-uhr.C:
				v.Verzeichnis = verzeichnis.Load()
			case <-ctx.Done():
				return
			}
		}
	}()

	server := &http.Server{
		Addr:    adresse,
		Handler: v,
		// KEIN WriteTimeout. Ein Modellaufruf dauert 5-60 s, ein langer Strom laenger;
		// ein WriteTimeout schneidet ihn mitten im Satz ab.
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(protokoll.Handler(), slog.LevelWarn),
	}

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if err := ladeHaupt(); err != nil {
				protokoll.Error("hauptschluessel neu laden gescheitert", "fehler", err)
			}
			abgleicher.Einmal(ctx)
			v.Verzeichnis = verzeichnis.Load()
			protokoll.Info("neu geladen", "schluessel", len(v.Verzeichnis.Alle()))
		}
	}()

	go func() {
		<-ctx.Done()
		// Laufende Stroeme duerfen zu Ende laufen: ein Modellaufruf, der bei einem
		// Neustart abbricht, ist bezahlte Arbeit, die niemand bekommt.
		frist, abbrechen := context.WithTimeout(context.Background(), 90*time.Second)
		defer abbrechen()
		_ = server.Shutdown(frist)
	}()

	anzahl, geholt := tafel.Stand()
	protokoll.Info("vorbau laeuft",
		"adresse", adresse, "ziel", ziel.String(),
		"schluessel", len(v.Verzeichnis.Alle()), "toepfe", len(toepfe.Alle()),
		"preise", anzahl, "preise_geholt", geholt.Format(time.RFC3339),
		"aufschlag_prozent", float64(aufschlag)/100, "boden_usd", boden.Text(2))

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Erst den Kanal schliessen, dann auf den Schreiber warten: was noch drin steht,
	// gehoert in die Abrechnung.
	close(buch)
	select {
	case <-fertig:
	case <-time.After(45 * time.Second):
		protokoll.Error("der schreiber wurde nicht fertig — abrechnung kann fehlen")
	}
	protokoll.Info("vorbau beendet")
	return nil
}

func holePreiseRegelmaessig(ctx context.Context, tafel *preise.Liste, client *http.Client,
	basis string, schluessel *atomic.Pointer[string], protokoll *slog.Logger) {
	uhr := time.NewTicker(6 * time.Hour)
	defer uhr.Stop()
	for {
		select {
		case <-uhr.C:
			if err := tafel.Hole(ctx, client, basis, *schluessel.Load()); err != nil {
				protokoll.Warn("preistafel nicht aufgefrischt", "fehler", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func neuerClient() *http.Client {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		// Die Vorgabe ist 2. Bei 20-30 gleichzeitigen Stroemen auf denselben Host baut Go
		// sonst staendig neue Verbindungen auf und zahlt je Strom einen TLS-Handschlag.
		MaxIdleConnsPerHost: 64,
		MaxIdleConns:        128,
		// Der Kopf einer nicht-stroemenden Antwort kommt erst, wenn das Modell fertig ist
		// — also nach bis zu 60 s.
		ResponseHeaderTimeout: 180 * time.Second,
		// OHNE DIESE ZEILE IST "UNVERAENDERT" GELOGEN. Go setzt sonst selbst
		// `Accept-Encoding: gzip`, packt die Antwort still aus und liefert andere Bytes,
		// als OpenRouter geschickt hat.
		DisableCompression: true,
	}
	return &http.Client{
		Transport: transport,
		// KEIN Timeout. Er gilt fuer die GANZE Anfrage samt Koerper und wuerde jeden
		// langen Strom kappen.
		Timeout: 0,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// leseHauptschluessel akzeptiert beides: eine Datei, die nur den Schluessel enthaelt, und
// eine env-Datei mit OPENROUTER_API_KEY=... — Letzteres ist die Form, die klabs auf diesem
// Server schon benutzt.
func leseHauptschluessel(pfad string) (string, error) {
	roh, err := os.ReadFile(pfad)
	if err != nil {
		return "", fmt.Errorf("hauptschluessel lesen: %w", err)
	}
	for _, zeile := range strings.Split(string(roh), "\n") {
		zeile = strings.TrimSpace(zeile)
		if zeile == "" || strings.HasPrefix(zeile, "#") {
			continue
		}
		if wert, gefunden := strings.CutPrefix(zeile, "OPENROUTER_API_KEY="); gefunden {
			return strings.Trim(strings.TrimSpace(wert), `"'`), nil
		}
		if !strings.Contains(zeile, "=") {
			return zeile, nil
		}
	}
	return "", fmt.Errorf("in %s steht kein Schluessel (erwartet: OPENROUTER_API_KEY=... oder der Schluessel allein)", pfad)
}

func schluesselVerwalten(argumente []string) error {
	if len(argumente) == 0 {
		return errors.New("aufruf: kirla-api schluessel neu <name> | liste | sperren <kennung> | oeffnen <kennung> | uebernehmen")
	}
	ctx := context.Background()
	abl, err := oeffneAblage(ctx)
	if err != nil {
		return err
	}
	defer abl.Schliesse()

	switch argumente[0] {
	case "neu":
		if len(argumente) < 2 {
			return errors.New("aufruf: kirla-api schluessel neu <name>")
		}
		klartext, eintrag, err := schluessel.Erzeuge(argumente[1])
		if err != nil {
			return err
		}
		if err := abl.LegeSchluesselAn(ctx, eintrag, 0); err != nil {
			return err
		}
		fmt.Printf("Schluessel fuer %q — er wird JETZT EINMAL gezeigt und danach nie wieder:\n\n  %s\n\n", eintrag.Nutzer, klartext)
		fmt.Printf("Kennung: %s\n", eintrag.Kennung)
		fmt.Printf("Der Topf ist LEER. Ohne Guthaben antwortet die Schnittstelle mit 402:\n")
		fmt.Printf("  kirla-api topf aufladen %s 10\n", eintrag.Kennung)
		return nil

	case "liste":
		liste, err := abl.LadeSchluessel(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%-14s %-22s %-9s %14s  %s\n", "KENNUNG", "NUTZER", "ZUSTAND", "GUTHABEN USD", "ANGELEGT")
		for _, s := range liste {
			zustand := "offen"
			if s.Eintrag.Gesperrt {
				zustand = "gesperrt"
			}
			fmt.Printf("%-14s %-22s %-9s %14s  %s\n", s.Eintrag.Kennung, s.Eintrag.Nutzer,
				zustand, s.Guthaben.Text(6), s.Eintrag.Angelegt.Format("2006-01-02 15:04"))
		}
		return nil

	case "sperren", "oeffnen":
		if len(argumente) < 2 {
			return fmt.Errorf("aufruf: kirla-api schluessel %s <kennung>", argumente[0])
		}
		if err := abl.Sperre(ctx, argumente[1], argumente[0] == "sperren"); err != nil {
			return err
		}
		fmt.Printf("%s: %s\n", argumente[1], map[bool]string{true: "gesperrt", false: "offen"}[argumente[0] == "sperren"])
		fmt.Println("Wirksam im laufenden Dienst nach dem naechsten Abgleich (30 s) oder sofort mit `systemctl reload kirla-api`.")
		return nil

	case "uebernehmen":
		// Einmalig: die Schluessel aus der Datei von Schritt 1 in die Datenbank holen.
		datei := umgebung("KIRLA_API_SCHLUESSEL", vorgabeSchluesselDatei)
		v, err := schluessel.Lade(datei)
		if err != nil {
			return err
		}
		vorhanden, err := abl.LadeSchluessel(ctx)
		if err != nil {
			return err
		}
		schon := map[string]bool{}
		for _, s := range vorhanden {
			schon[s.Eintrag.Kennung] = true
		}
		uebernommen := 0
		for _, e := range v.Alle() {
			if schon[e.Kennung] {
				continue
			}
			if err := abl.LegeSchluesselAn(ctx, e, 0); err != nil {
				return fmt.Errorf("%s: %w", e.Kennung, err)
			}
			uebernommen++
			fmt.Printf("uebernommen: %s (%s), Topf leer\n", e.Kennung, e.Nutzer)
		}
		fmt.Printf("%d von %d Schluesseln uebernommen; %s kann danach weg.\n", uebernommen, len(v.Alle()), datei)
		return nil
	}
	return fmt.Errorf("unbekannt: %q", argumente[0])
}

func topfVerwalten(argumente []string) error {
	if len(argumente) < 3 || argumente[0] != "aufladen" {
		return errors.New("aufruf: kirla-api topf aufladen <kennung> <USD>")
	}
	betrag, err := geld.AusText(argumente[2])
	if err != nil {
		return fmt.Errorf("betrag %q: %w", argumente[2], err)
	}
	ctx := context.Background()
	abl, err := oeffneAblage(ctx)
	if err != nil {
		return err
	}
	defer abl.Schliesse()

	neu, err := abl.Aufladen(ctx, argumente[1], betrag)
	if err != nil {
		return err
	}
	fmt.Printf("%s: %s USD aufgeladen, neues Guthaben %s USD\n",
		argumente[1], betrag.Text(6), neu.Text(6))
	fmt.Println("Wirksam im laufenden Dienst nach dem naechsten Abgleich (30 s) oder sofort mit `systemctl reload kirla-api`.")
	return nil
}
