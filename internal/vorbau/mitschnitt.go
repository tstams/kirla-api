package vorbau

import (
	"bytes"
	"compress/gzip"
	"time"

	"github.com/tstams/kirla-api/internal/ablage"
)

// VorgabeMitschnittgrenze ist, wie viel je Richtung hoechstens mitgeschnitten wird.
//
// Der Auftrag §5 sagt "Anfrage und Antwort VOLLSTAENDIG" und rechnet mit ~15 KB je Aufruf.
// 2 MB sind das Hundertdreissigfache davon -- praktisch immer vollstaendig. Eine Grenze
// braucht es trotzdem, und zwar aus demselben Grund wie ueberall in diesem Dienst: ohne sie
// haengt der Speicher an der Laenge einer fremden Anfrage. Bei 30 gleichzeitigen Stroemen
// (§6) waeren 2 MB je Richtung schon 120 MB; der Deckel der Slice steht auf 512 MB.
//
// Wird gekuerzt, steht das im Satz (`gekuerzt`) und nicht nur im Kopf des Lesers.
const VorgabeMitschnittgrenze = 2 << 20

// mitschnittPuffer sammelt bis zu max Bytes und zaehlt, wie viel wirklich kam.
type mitschnittPuffer struct {
	inhalt []byte
	max    int
	gesamt int64
}

func neuerMitschnittPuffer(max int) *mitschnittPuffer {
	if max <= 0 {
		max = VorgabeMitschnittgrenze
	}
	return &mitschnittPuffer{max: max}
}

func (p *mitschnittPuffer) Write(d []byte) (int, error) {
	p.gesamt += int64(len(d))
	if platz := p.max - len(p.inhalt); platz > 0 {
		if platz > len(d) {
			platz = len(d)
		}
		p.inhalt = append(p.inhalt, d[:platz]...)
	}
	// IMMER die volle Laenge melden. Ein TeeReader bricht ab, wenn der Mitschreiber
	// weniger nimmt als angeboten -- dann fehlte OpenRouter der Rest der Anfrage.
	return len(d), nil
}

func (p *mitschnittPuffer) gekuerzt() bool { return p.gesamt > int64(len(p.inhalt)) }

// packe komprimiert fuer die Ablage. Abgelegt wird immer gzip ueber Klartext, damit fuer
// jeden Satz dieselbe Regel gilt: `gunzip`, und man liest, was auf der Leitung stand.
func packe(roh []byte) []byte {
	if len(roh) == 0 {
		return nil
	}
	var aus bytes.Buffer
	packer, err := gzip.NewWriterLevel(&aus, gzip.BestSpeed)
	if err != nil {
		return nil
	}
	if _, err := packer.Write(roh); err != nil {
		_ = packer.Close()
		return nil
	}
	if err := packer.Close(); err != nil {
		return nil
	}
	return aus.Bytes()
}

// lege schiebt den Mitschnitt in den Kanal, OHNE zu blockieren.
//
// Derselbe Vorrang wie bei den Kopfdaten (§0): laeuft der Kanal voll, wird der Mitschnitt
// verworfen und protokolliert. Ein Mitschnitt ist Diagnose; ihn zu verlieren kostet die
// Fehlersuche, aber nicht die Abrechnung -- und ein blockierter Anfrageweg kostet die
// Verfuegbarkeit jeder Kundenfirma.
func (v *Vorbau) lege(anfrageID string, zeit time.Time, anfrage, antwort []byte, kamAls string, gekuerzt bool) {
	if v.Ablagekanal == nil {
		return
	}
	i := ablage.Inhalt{
		AnfrageID: anfrageID, Zeit: zeit,
		Anfrage: packe(anfrage), Antwort: packe(antwort),
		KamAls: kamAls, Gekuerzt: gekuerzt,
	}
	select {
	case v.Ablagekanal <- i:
	default:
		v.Protokoll.Warn("mitschnitt verworfen — der schreiber kommt nicht nach",
			"anfrage_id", anfrageID)
	}
}
