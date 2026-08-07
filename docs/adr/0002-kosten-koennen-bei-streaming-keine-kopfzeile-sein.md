# ADR-0002: Die Kostenangabe kann bei Streaming keine Kopfzeile sein

## Status

Angenommen (07.08.2026). **Befund am Bauauftrag** `docs/kirla-api-handoff.md` §3: die dort
verlangte Auskunft ist in der verlangten Form für den halben Verkehr nicht lieferbar. Diese
Entscheidung hält fest, was in Schritt 1 gebaut wurde und was in Schritt 2 zu klären ist.

## Kontext

Der Auftrag verlangt drei zusätzliche Kopfzeilen in jeder Antwort:

| Kopfzeile | Zweck |
|---|---|
| `X-Kirla-Kosten-USD` | was dieser Aufruf gekostet hat (aus OpenRouters `usage.cost`) |
| `X-Kirla-Guthaben-USD` | was im Topf danach übrig ist |
| `X-Kirla-Anfrage-Id` | Kennung für Rückfragen |

Derselbe Auftrag verlangt in §3, **Streaming sei Pflicht, nicht Kür**, und in §1, die
Antwort werde **unverändert** durchgereicht.

Diese drei Festlegungen sind zusammen nicht erfüllbar. HTTP schickt den Kopf **vor** dem
Körper. Bei `"stream": true` kommt `usage.cost` im **letzten** Teilstück des
`text/event-stream` — also Sekunden bis Minuten, nachdem der Kopf längst beim Kunden ist.
Zu dem Zeitpunkt, an dem `X-Kirla-Kosten-USD` gesetzt werden müsste, weiß niemand, was der
Aufruf kostet.

Es gibt genau drei Auswege, und alle drei haben einen Preis:

* **Den Strom puffern, bis die Kosten feststehen.** Nimmt dem Vorbau die Eigenschaft, die
  ihn billig macht, und ist ausdrücklich verboten (§3).
* **HTTP-Trailer.** Genau dafür gemacht — und in der Praxis von Zwischenstellen, Browsern
  und den meisten Client-Bibliotheken nicht gelesen. Eine Auskunft, die niemand empfängt.
* **Die Kosten dort ausweisen, wo sie ankommen** — nicht im Kopf.

## Entscheidung

**`X-Kirla-Anfrage-Id` steht ab Schritt 1 in jeder Antwort**, auch in jeder abgelehnten. Sie
hängt an nichts und ist genau dann zur Stelle, wenn jemand eine Rückfrage hat.

**`X-Kirla-Kosten-USD` und `X-Kirla-Guthaben-USD` werden in Schritt 1 nicht gesetzt.** In
Schritt 1 gibt es keine Töpfe, also gäbe es auch nichts auszuweisen. Der Quelltext benennt
die Stelle, an der sie fehlen, samt Grund.

**Für Schritt 2 ist die Regel: bei nicht-strömenden Antworten als Kopfzeile, bei strömenden
gar nicht** — und die Abrechnung wird stattdessen dort nachlesbar, wo sie ohnehin hingehört:
in den Kopfdaten in der Datenbank, abrufbar über die Anfrage-Id. Das ist keine Notlösung sondern
die ehrlichere Stelle: eine Kopfzeile, die nur bei der Hälfte der Aufrufe erscheint, ist als
Abrechnungsgrundlage schlechter als eine Zeile, die es immer gibt.

## Alternativen

**Ein zusätzliches SSE-Ereignis am Ende des Stroms** (`event: kirla-kosten`). Technisch
sauber und beim Kunden zuverlässig lesbar — aber es **verändert den Strom**, und genau das
verbietet §1. klabs würde ein unbekanntes Ereignis vermutlich überlesen; „vermutlich" ist
bei der Namensbrücke die falsche Sicherheit.

**Immer puffern und die Kür aufgeben.** Wurde verworfen: der Auftrag begründet das Streaming
nicht mit Bequemlichkeit, sondern mit dem Speicher je Anfrage bei langen Läufen.

## Folgen

**Der Preis:**

* **Wer bei einem Streaming-Aufruf die Kosten wissen will, muss sie nachschlagen** statt sie
  in der Antwort zu finden — über `X-Kirla-Anfrage-Id`. Ein Schritt mehr für den Menschen,
  der eine Rechnung nachvollzieht.
* **Die Auskunft ist uneinheitlich**: derselbe Aufruf mit und ohne `stream` antwortet
  verschieden. Das ist unschön und wird im Quelltext an beiden Stellen benannt, damit es
  niemand später für einen Fehler hält und „repariert".

**Was besser wird:** der Widerspruch ist entschieden, bevor er gebaut wurde. klabs braucht
diese Kopfzeilen heute nicht (§3: „klabs braucht sie heute nicht — es rechnet lokal in
`state/ausgaben.jsonl`"), also kostet die Verzögerung auf Schritt 2 nichts.

**Was dem Betreiber gehört:** ob die spätere Anzeige die Kosten aus der Datenbank holt (wie
hier vorgesehen) oder ob sie doch eine Kopfzeile bei nicht-strömenden Aufrufen erwartet. Das
entscheidet sich, wenn es die Anzeige gibt — nicht vorher.
