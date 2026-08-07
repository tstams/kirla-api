# ADR-0003: „Kostenlos" und „keine Kostenangabe" sind zwei Fälle, nicht einer

## Status

Angenommen (07.08.2026). Präzisiert `docs/kirla-api-handoff.md` §4 aus dem klabs-Repositorium
an einer Stelle, an der der Auftrag zwei verschiedene Lagen zu einer zusammenfasst.

## Kontext

Der Auftrag §4 sagt:

> **Meldet OpenRouter keine Kosten, wird ein Boden gebucht und nicht null.** Sonst ist jedes
> Modell ohne Preisangabe gratis, und genau so entsteht eine Rechnung, die niemand erwartet
> hat. klabs nennt diesen Wert `UnbekanntUSD`; nehmen Sie denselben (2 ¢) als Vorgabe.

Die Regel ist richtig und der Grund ist richtig. Nur trägt sie eine Annahme, die sich beim
ersten echten Aufruf als falsch erwiesen hat: dass „keine Kosten gemeldet" ein einziger
Zustand ist.

Gemessen am 07.08.2026 gegen echtes OpenRouter, Modell `inclusionai/ling-3.0-tiny:free`:

```json
"usage": {"prompt_tokens": 246, "completion_tokens": 48, "cost": 0, "is_byok": false}
```

Das Feld ist **da** und steht ausdrücklich auf null. Das ist keine fehlende Angabe, sondern
eine Angabe — die Antwort hat wirklich nichts gekostet. OpenRouter führte an diesem Tag
14 solche Modelle.

Wer beides gleich behandelt, bucht für jeden Aufruf eines kostenlosen Modells 2 ¢ plus
Aufschlag. Bei den 10 000 Aufrufen am Tag aus §5 wären das **230 $ am Tag** für Leistungen,
die der Betreiber selbst geschenkt bekommt. Das ist genau die Sorte Rechnung, die derselbe
Paragraph verhindern will — nur in die andere Richtung.

## Entscheidung

Drei Fälle statt zwei, festgehalten in der Spalte `kosten_quelle` der Tabelle `aufruf`:

| Wert | Lage | gebucht wird |
|---|---|---:|
| `gemeldet` | `usage.cost` ist da und größer als null | die gemeldete Zahl |
| `frei` | `usage.cost` ist da und **genau null** | **0** |
| `boden` | `usage.cost` fehlt, ist `null` oder unlesbar | der Boden (2 ¢) |

Dazu ein vierter, der aus derselben Überlegung folgt: **eine Antwort mit Statuscode ≥ 400
kostet nichts.** Bei OpenRouter entstehen für einen abgelehnten Aufruf keine Kosten; ihm
einen Boden aufzuerlegen hieße, Fehler in Rechnung zu stellen.

Die Unterscheidung steht in `internal/kosten` und ist durch Tests gedeckt, die genau die
vier Gestalten einsetzen.

## Alternativen

**Alles ohne positive Kostenangabe bekommt den Boden** — der Wortlaut des Auftrags. Verworfen:
er stellt Gratismodelle in Rechnung, und der Kunde kann es nachrechnen.

**Alles ohne positive Kostenangabe ist gratis.** Genau der Fehler, den §4 benennt: ein Modell,
dessen Preis kirla.ai nicht kennt, wäre umsonst, und der Betreiber zahlt.

**Nur Modelle mit `:free` im Namen sind frei.** Eine Namenskonvention als Abrechnungsgrundlage.
Sie hält genau so lange, wie OpenRouter sie beibehält, und bricht still.

## Folgen

**Der Preis:** ein Anbieter, der versehentlich `cost: 0` statt gar nichts meldet, wird nicht
berechnet. Das ist die Richtung, in die dieser Fehler gehen soll — der Betreiber verliert
einen Bruchteil eines Cents, statt einen Kunden für eine Leistung zu belasten, die dieser
nie bekommen hat.

**Was besser wird:** die Rechnung sagt hinterher, WELCHER Fall es war. `kosten_quelle` steht
in den Kopfdaten und dauerhaft in der Datenbank. Häufen sich `boden`-Sätze, ist das ein
Befund — entweder kennt kirla.ai einen Preis nicht, oder OpenRouter hat sein Antwortformat
geändert. Ohne die Spalte wäre beides unsichtbar.

**Offen und dem Betreiber gehörig:** ein Schlüssel mit leerem Topf kann kostenlose Modelle
weiter benutzen — die Schätzung ist null, also greift der `402` nicht. Das ist in sich
stimmig (ein Aufruf, der nichts kostet, darf nicht am Guthaben scheitern), aber es ist eine
Geschäftsentscheidung und keine technische. Wer das nicht will, braucht eine Mindestrücklage
je Aufruf statt einer reinen Kostenschätzung.
