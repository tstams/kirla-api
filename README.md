# kirla-api — die Zwischenstelle

Ein HTTP-Dienst, der die OpenAI-kompatible Schnittstelle spricht, einen `kirla_sk_…`-Schlüssel
prüft und die Anfrage **unverändert** an OpenRouter weiterreicht.

Der Bauauftrag liegt im klabs-Repositorium: `docs/kirla-api-handoff.md` und
`docs/adr/0016-der-schluessel-bindet-nicht-das-binary.md`. Dieses Repositorium ist ein
**eigenständiges Produkt** und kein Teil von klabs.

## Die eine Regel

**Der heiße Weg darf nichts anfassen müssen.** Kein Datenbankschreibvorgang im Anfrageweg,
kein Stripe-Aufruf, keine Migration, die ihn anhält. Fällt alles hinter dem Vorbau aus, reicht
er weiter. Daran hängt die Verfügbarkeit jeder Kundenfirma.

Die zweite Regel: **unverändert heißt wörtlich.** Werkzeugdefinitionen, `tool_choice`,
`response_format`, Streaming, Modellname. klabs schickt Werkzeuge als `fs_read` und erwartet
sie genauso zurück; wer hier normalisiert, bricht die Namensbrücke im Adapter.

## Stand

| Schritt | Was | Zustand |
|---|---|---|
| 1 | Durchreichen + Schlüsselprüfung | **gebaut** |
| 2 | Kopfdaten buchen, Guthabentöpfe, `402` | **gebaut** |
| 3 | Kulanzfenster 24 h | **gebaut** |
| 4 | Ablage 30 Tage komprimiert + `GET /v1/route` | **gebaut** |
| 5 | Stripe-Aufladung | offen |

## Bauen und prüfen

```bash
go build ./... && go vet ./... && go test ./...
```

## Betrieb

```bash
kirla-api dienst                 # den Vorbau starten
kirla-api schluessel neu <name>  # einen Kundenschlüssel vergeben (zeigt ihn EINMAL)
kirla-api schluessel liste       # vergebene Schlüssel zeigen (ohne Geheimnis)
kirla-api topf aufladen <kennung> <USD>
kirla-api mitschnitt <anfrage-id>  # Anfrage und Antwort eines Aufrufs zeigen
kirla-api verfall                  # abgelaufene Mitschnitte jetzt löschen
```

## Was gespeichert wird

**Anfrage und Antwort vollständig, 30 Tage, komprimiert** — darin stehen die Systemtexte,
der Gesprächsverlauf und die Antworten des Modells jeder Kundenfirma. Danach gelöscht.
Die **Kopfdaten** daneben (Zeit, Schlüssel, Modell, Tokens, Kosten, Dauer, Status) bleiben
**dauerhaft**; sie sind Abrechnung und Zeitreihe.

Nachlesbar ist das unter `GET /v1/route` — und genau dafür gibt es den Endpunkt: eine
Zwischenstelle, deren Umfang man nicht nachlesen kann, ist eine Wette.

Einstellungen über die Umgebung:

| Name | Vorgabe | Wofür |
|---|---|---|
| `KIRLA_API_ADRESSE` | `127.0.0.1:3303` | worauf gelauscht wird (Bereich 3303–3349 laut REGISTRY.md; 8080 ist gesperrt) |
| `KIRLA_API_ZIEL` | `https://openrouter.ai/api/v1` | wohin weitergereicht wird |
| `KIRLA_API_SCHLUESSEL` | `/srv/kirla-labs/dev/secrets/kirla-schluessel.json` | die vergebenen Kundenschlüssel (nur Hashes) |
| `KIRLA_API_HAUPTSCHLUESSEL` | `/srv/kirla-labs/dev/secrets/openrouter.env` | der OpenRouter-Hauptschlüssel, Modus 0600 |
| `KIRLA_API_NACHBUCH` | `/srv/kirla-labs/nachbuch/offen.jsonl` | Abrechnung, die während eines Datenbankausfalls anfällt |
| `KIRLA_API_KULANZ` | `24h` | wie lange der Vorbau ohne Datenbank weiterreicht |
| `KIRLA_API_AUFBEWAHRUNG` | `720h` | wie lange die Mitschnitte liegen (30 Tage) |

`SIGHUP` lädt Hauptschlüssel und Schlüsselablage neu, ohne den Dienst anzuhalten — ein
Schlüsseltausch braucht keinen Neustart.

## Prüfen ohne Kunden

klabs zeigt über `CEO_LLM_BASE_URL` hierher. **Zwei Fallen, beide gemessen:**

* Die Basis-URL steht **ohne `/v1`** — klabs hängt `/v1/chat/completions` selbst an
  (`core/openrouter/openrouter.go:299`). Mit `/v1` entsteht `/v1/v1/…` und ein 404, der wie
  ein Fehler der Zwischenstelle aussieht.
* Der Schlüssel steht in **`OPENROUTER_API_KEY`**, nicht in `CEO_LLM_API_KEY` — auch wenn ein
  `kirla_sk_…` drin liegt (`core/cmd/klabs/agent.go`, `schluesselHolen`). klabs liest zuerst
  die Umgebung, dann `$HOME/dev/secrets/openrouter.env`. `CEO_LLM_BASE_URL` dagegen kommt
  **nur** aus der Umgebung — wer nur die Datei ändert und von Hand startet, landet weiter bei
  OpenRouter.

```bash
CEO_LLM_BASE_URL=https://api.kirla.ai \
OPENROUTER_API_KEY=kirla_sk_… \
klabs lead
```

Die Testdaten stammen aus `core/openrouter/openrouter_test.go` in klabs — aus echten Fehlern,
nicht aus der Vorstellung. Der wichtigste Fall ist eine Antwort, deren
`tool_calls.function.arguments` mitten im JSON abbricht: sie geht **unverändert** durch und
wird nicht repariert.

## Entscheidungen

- [ADR-0001](docs/adr/0001-die-gestalt-des-kundenschluessels.md) — der Kundenschlüssel trägt
  eine Kennung, und gehasht wird mit bcrypt
- [ADR-0002](docs/adr/0002-kosten-koennen-bei-streaming-keine-kopfzeile-sein.md) — die
  Kostenangabe kann bei Streaming keine Kopfzeile sein
- [ADR-0003](docs/adr/0003-frei-ist-nicht-unbekannt.md) — „kostenlos" und „keine
  Kostenangabe" sind zwei Fälle, nicht einer
