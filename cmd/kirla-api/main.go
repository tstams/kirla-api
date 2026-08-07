// kirla-api ist die Zwischenstelle zwischen einer klabs-Instanz und OpenRouter.
//
// Aufruf:
//
//	kirla-api dienst                 den Vorbau starten
//	kirla-api schluessel neu <name>  einen Kundenschluessel vergeben (zeigt ihn EINMAL)
//	kirla-api schluessel liste       vergebene Schluessel zeigen (ohne Geheimnis)
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

	"github.com/tstams/kirla-api/internal/schluessel"
	"github.com/tstams/kirla-api/internal/vorbau"
)

func main() {
	protokoll := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "aufruf: kirla-api dienst | schluessel neu <name> | schluessel liste")
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "dienst":
		err = dienst(protokoll)
	case "schluessel":
		err = schluesselVerwalten(os.Args[2:])
	default:
		err = fmt.Errorf("unbekannter befehl %q", os.Args[1])
	}
	if err != nil {
		protokoll.Error("abbruch", "fehler", err)
		os.Exit(1)
	}
}

// umgebung liest eine Einstellung mit Vorgabe.
func umgebung(name, vorgabe string) string {
	if wert := os.Getenv(name); wert != "" {
		return wert
	}
	return vorgabe
}

func dienst(protokoll *slog.Logger) error {
	// 3303 und nicht 8080: auf kirla-01 steht 8080 ausdruecklich auf der Freilassliste
	// (/etc/kirla/REGISTRY.md §3), 3303-3349 ist der fuer kirla.ai reservierte Bereich.
	// Die Vorgabe im Quelltext folgt der Unit, damit ein Lauf von Hand nicht auf einem
	// anderen Port landet als der Dienst.
	adresse := umgebung("KIRLA_API_ADRESSE", "127.0.0.1:3303")
	zielRoh := umgebung("KIRLA_API_ZIEL", "https://openrouter.ai/api/v1")
	schluesselDatei := umgebung("KIRLA_API_SCHLUESSEL", "/srv/kirla-labs/dev/secrets/kirla-schluessel.json")
	hauptDatei := umgebung("KIRLA_API_HAUPTSCHLUESSEL", "/srv/kirla-labs/dev/secrets/openrouter.env")

	ziel, err := url.Parse(zielRoh)
	if err != nil {
		return fmt.Errorf("KIRLA_API_ZIEL: %w", err)
	}

	verzeichnis, err := schluessel.Lade(schluesselDatei)
	if err != nil {
		return err
	}

	// Der Hauptschluessel steht im Arbeitsspeicher und wird NICHT bei jeder Anfrage von
	// der Platte gelesen. Ein Lesevorgang im Anfrageweg heisst: haengt die Platte, haengt
	// der Dienst — genau das, was der Auftrag §0 ausschliesst. Ein SIGHUP laedt neu,
	// damit ein Schluesseltausch keinen Neustart braucht.
	var hauptschluessel atomic.Pointer[string]
	laden := func() error {
		wert, err := leseHauptschluessel(hauptDatei)
		if err != nil {
			return err
		}
		hauptschluessel.Store(&wert)
		return nil
	}
	if err := laden(); err != nil {
		return err
	}

	v := &vorbau.Vorbau{
		Ziel:            ziel,
		Hauptschluessel: func() string { return *hauptschluessel.Load() },
		Verzeichnis:     verzeichnis,
		Client:          neuerClient(),
		Protokoll:       protokoll,
	}

	server := &http.Server{
		Addr:    adresse,
		Handler: v,
		// KEIN WriteTimeout. Ein Modellaufruf dauert 5-60 s, ein langer Strom laenger;
		// ein WriteTimeout schneidet ihn mitten im Satz ab, und der Fehler sieht aus wie
		// ein Problem des Anbieters.
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(protokoll.Handler(), slog.LevelWarn),
	}

	ende := make(chan os.Signal, 1)
	signal.Notify(ende, syscall.SIGINT, syscall.SIGTERM)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if err := laden(); err != nil {
				protokoll.Error("hauptschluessel neu laden gescheitert", "fehler", err)
				continue
			}
			neu, err := schluessel.Lade(schluesselDatei)
			if err != nil {
				protokoll.Error("schluesselablage neu laden gescheitert", "fehler", err)
				continue
			}
			v.Verzeichnis = neu
			protokoll.Info("neu geladen", "schluessel", len(neu.Alle()))
		}
	}()

	go func() {
		<-ende
		// Laufende Stroeme duerfen zu Ende laufen: ein Modellaufruf, der bei einem
		// Neustart abbricht, ist bezahlte Arbeit, die niemand bekommt.
		frist, abbrechen := context.WithTimeout(context.Background(), 90*time.Second)
		defer abbrechen()
		_ = server.Shutdown(frist)
	}()

	protokoll.Info("vorbau laeuft",
		"adresse", adresse, "ziel", ziel.String(), "schluessel", len(verzeichnis.Alle()))
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	protokoll.Info("vorbau beendet")
	return nil
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
		// — also nach bis zu 60 s. Grosszuegig, aber nicht unendlich: ohne Grenze bleibt
		// eine tote Verbindung fuer immer stehen.
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
	datei := umgebung("KIRLA_API_SCHLUESSEL", "/srv/kirla-labs/dev/secrets/kirla-schluessel.json")
	if len(argumente) == 0 {
		return errors.New("aufruf: kirla-api schluessel neu <name> | schluessel liste")
	}

	switch argumente[0] {
	case "neu":
		if len(argumente) < 2 {
			return errors.New("aufruf: kirla-api schluessel neu <name>")
		}
		bestand := []schluessel.Eintrag{}
		if v, err := schluessel.Lade(datei); err == nil {
			bestand = v.Alle()
		} else if !os.IsNotExist(errors.Unwrap(err)) && !os.IsNotExist(err) {
			return err
		}
		klartext, eintrag, err := schluessel.Erzeuge(argumente[1])
		if err != nil {
			return err
		}
		if err := schluessel.Sichere(datei, append(bestand, eintrag)); err != nil {
			return err
		}
		fmt.Printf("Schluessel fuer %q — er wird JETZT EINMAL gezeigt und danach nie wieder:\n\n  %s\n\n", eintrag.Nutzer, klartext)
		fmt.Printf("Kennung: %s   Ablage: %s\n", eintrag.Kennung, datei)
		return nil

	case "liste":
		v, err := schluessel.Lade(datei)
		if err != nil {
			return err
		}
		for _, e := range v.Alle() {
			zustand := "offen"
			if e.Gesperrt {
				zustand = "gesperrt"
			}
			fmt.Printf("%s  %-20s %-8s %s\n", e.Kennung, e.Nutzer, zustand, e.Angelegt.Format(time.RFC3339))
		}
		return nil
	}
	return fmt.Errorf("unbekannt: %q", argumente[0])
}
