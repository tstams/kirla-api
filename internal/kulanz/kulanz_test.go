package kulanz

import (
	"testing"
	"time"
)

// mitUhr baut eine Lage mit stellbarer Zeit -- warten waere hier keine Pruefung, sondern
// eine Wette auf den Zeitplaner.
func mitUhr(fenster time.Duration) (*Lage, func(time.Duration)) {
	jetzt := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	l := Neu(fenster)
	l.jetzt = func() time.Time { return jetzt }
	return l, func(d time.Duration) { jetzt = jetzt.Add(d) }
}

// OHNE EINEN EINZIGEN ERFOLG IST ES KEIN KULANZFALL.
//
// Ein Dienst, dessen Datenbank noch NIE geantwortet hat, kennt weder Schluessel noch
// Toepfe. Ihn als "im Fenster" zu fuehren hiesse, jeden Fremden durchzulassen -- die
// Kulanz soll den letzten bekannten Stand verlaengern, nicht einen fehlenden ersetzen.
func TestOhneErfolgIstEsKeinFenster(t *testing.T) {
	l, _ := mitUhr(VorgabeFenster)
	if z, _ := l.Zustand(); z != Abgelaufen {
		t.Fatalf("Zustand %v statt abgelaufen", z)
	}
	l.Gescheitert()
	if z, _ := l.Zustand(); z != Abgelaufen {
		t.Fatalf("nach einem Fehlversuch: %v", z)
	}
}

func TestSolangeDieDatenbankAntwortetIstAllesGesund(t *testing.T) {
	l, vor := mitUhr(VorgabeFenster)
	l.Gelungen()
	if z, _ := l.Zustand(); z != Gesund {
		t.Fatalf("Zustand %v", z)
	}
	// Auch nach langer Zeit gilt "gesund", SOLANGE der letzte Versuch gelungen ist --
	// der Abgleicher laeuft alle 30 s, ein alter Erfolg ohne Fehlversuch bedeutet also
	// nur, dass niemand nachgesehen hat.
	vor(48 * time.Hour)
	l.Gelungen()
	if z, _ := l.Zustand(); z != Gesund {
		t.Fatalf("Zustand %v", z)
	}
}

// DIE 24 STUNDEN SIND DIE GANZE ZUSAGE (ADR-0016).
func TestDasFensterTraegtVierundzwanzigStundenUndDannNichtMehr(t *testing.T) {
	l, vor := mitUhr(VorgabeFenster)
	l.Gelungen()

	// Jetzt faellt die Datenbank aus.
	l.Gescheitert()
	if z, _ := l.Zustand(); z != ImFenster {
		t.Fatalf("direkt nach dem Ausfall: %v", z)
	}

	vor(23*time.Hour + 59*time.Minute)
	l.Gescheitert()
	if z, alter := l.Zustand(); z != ImFenster {
		t.Fatalf("nach %v: %v — das Fenster ging zu frueh zu", alter, z)
	}
	if rest := l.Rest(); rest < time.Minute-time.Second || rest > time.Minute {
		t.Errorf("Rest %v statt etwa einer Minute", rest)
	}

	vor(2 * time.Minute)
	l.Gescheitert()
	if z, alter := l.Zustand(); z != Abgelaufen {
		t.Fatalf("nach %v: %v — das Fenster blieb zu lange offen", alter, z)
	}
	if l.Rest() != 0 {
		t.Errorf("Rest %v obwohl abgelaufen", l.Rest())
	}
}

// KOMMT DIE DATENBANK ZURUECK, IST DAS FENSTER WIEDER ZU -- und zwar sofort und
// vollstaendig, nicht anteilig.
func TestNachDerWiederkehrIstAllesWiederGesund(t *testing.T) {
	l, vor := mitUhr(VorgabeFenster)
	l.Gelungen()
	l.Gescheitert()
	vor(20 * time.Hour)
	l.Gescheitert()
	if z, _ := l.Zustand(); z != ImFenster {
		t.Fatalf("Zustand %v", z)
	}

	l.Gelungen()
	if z, alter := l.Zustand(); z != Gesund || alter != 0 {
		t.Fatalf("Zustand %v, Alter %v", z, alter)
	}
	// Und ein neuer Ausfall bekommt das volle Fenster, nicht den Rest des alten.
	l.Gescheitert()
	vor(23 * time.Hour)
	l.Gescheitert()
	if z, _ := l.Zustand(); z != ImFenster {
		t.Fatalf("das zweite Fenster war kuerzer: %v", z)
	}
}

// EIN KUERZERES FENSTER LAESST SICH EINSTELLEN -- fuer den Betrieb und fuer Tests.
func TestEinEigenesFensterGilt(t *testing.T) {
	l, vor := mitUhr(time.Hour)
	l.Gelungen()
	l.Gescheitert()
	vor(59 * time.Minute)
	if z, _ := l.Zustand(); z != ImFenster {
		t.Fatalf("Zustand %v", z)
	}
	vor(2 * time.Minute)
	if z, _ := l.Zustand(); z != Abgelaufen {
		t.Fatalf("Zustand %v", z)
	}
}
