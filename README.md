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
| 2 | Kopfdaten buchen, Guthabentöpfe, `402` | offen |
| 3 | Kulanzfenster 24 h | offen |
| 4 | Ablage 30 Tage komprimiert + `GET /v1/route` | offen |
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
```

Einstellungen über die Umgebung:

| Name | Vorgabe | Wofür |
|---|---|---|
| `KIRLA_API_ADRESSE` | `127.0.0.1:8080` | worauf gelauscht wird |
| `KIRLA_API_ZIEL` | `https://openrouter.ai/api/v1` | wohin weitergereicht wird |
| `KIRLA_API_SCHLUESSEL` | `/srv/kirla-labs/dev/secrets/kirla-schluessel.json` | die vergebenen Kundenschlüssel (nur Hashes) |
| `KIRLA_API_HAUPTSCHLUESSEL` | `/srv/kirla-labs/dev/secrets/openrouter.env` | der OpenRouter-Hauptschlüssel, Modus 0600 |

`SIGHUP` lädt Hauptschlüssel und Schlüsselablage neu, ohne den Dienst anzuhalten — ein
Schlüsseltausch braucht keinen Neustart.

## Prüfen ohne Kunden

klabs zeigt über `CEO_LLM_BASE_URL` hierher:

```bash
CEO_LLM_BASE_URL=http://localhost:8080/v1 \
CEO_LLM_API_KEY=kirla_sk_… \
klabs lead --instance <testinstanz>
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
