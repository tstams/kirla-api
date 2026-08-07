## 3c. `kirla-api` — die kirla.ai-Zwischenstelle (angelegt 2026-08-07)

Ein eigenständiges Produkt, **nicht** Teil von klabs: ein HTTP-Dienst, der die
OpenAI-Schnittstelle spricht, einen `kirla_sk_…`-Schlüssel prüft und die Anfrage unverändert
an OpenRouter weiterreicht. Bauauftrag und Begründung liegen im klabs-Repositorium
(`docs/kirla-api-handoff.md`, `docs/adr/0016-…`); der Quelltext in `tstams/kirla-api`.

| Spalte | `kirla-api` |
|---|---|
| **Name** | **kirla.ai-Zwischenstelle** (`kirla-api`). Zone **`kirla.ai`**, Name `api.kirla.ai` |
| **Systembenutzer** | `labs` — Betreiberentscheidung 2026-08-07: *„labs ist der User für alles unter kirla.ai"*. Kein neuer Benutzer. Geprüft: `leuchtfeuer` (uid 1002) ist in keiner Gruppe von `labs` und bekommt auf `/srv/kirla-labs` ein `Permission denied` |
| **Deploy-Root** | Binary `/usr/local/bin/kirla-api` (root:root 0755). Zustand und Geheimnisse unter `/srv/kirla-labs/dev/secrets/` (0700 `labs:labs`) |
| **Slice** | **`kirla-labs-api.slice`** — Kind von `kirla-labs.slice`. `MemoryHigh=256M`, `MemoryMax=512M`, `MemorySwapMax=0`, `CPUQuota=200%`, `TasksMax=512`. Gemessener Verbrauch im Betrieb: **2,1 M** |
| **Ports** | **`3303`** auf `127.0.0.1` (Bereich 3303–3349). **Er lief zunächst auf `8080` — das war ein Verstoß gegen §3** („bewusst unbelegt lassen", zweifach begründet in B-33) und ist am 2026-08-07 korrigiert worden. `8080` ist wieder frei |
| **DB-Cluster / Port / DB / Rolle** | Cluster `labs` · `5442` · DB `labs` · Rolle `labs_app` — ab Schritt 2 (Kopfdaten, Guthabentöpfe). Schritt 1 kommt **ohne** Datenbank aus, und das ist Absicht: der heiße Weg darf nichts anfassen müssen |
| **vhost-Dateien** | `api.kirla.ai.conf` (Klasse a), verlinkt nach `sites-enabled`. **Folgt dem globalen Zugriffsschalter NICHT** — `public.conf` ist fest verdrahtet, weil ein `maintenance` für Chronicle sonst jeder Kundenfirma den Modellzugang nähme. Preis: wer den Host in Wartung legt, muss `systemctl stop kirla-api` einzeln fahren |
| **Zertifikats-Linie** | **Linie 6** — eigene Linie `api.kirla.ai`, ausgestellt 2026-08-07 über dns-cloudflare, ECDSA, gültig bis **2026-11-05**. Erneuerung über den globalen Deploy-Hook, Probelauf bestanden. Kein SAN mit Linie 4 (`kirla.ai`): das Frontend bleibt statisch, die Schnittstelle hat einen eigenen Lebenszyklus |
| **Backup** | **OFFEN (E-11)** — bis Schritt 2 gibt es nur `/srv/kirla-labs/dev/secrets/kirla-schluessel.json` (Hashes vergebener Schlüssel, ~260 Byte) und den Hauptschlüssel. **Beide gehören ins Backup**, sobald der Präfix entschieden ist. Ab Schritt 2 kommt die DB `labs` dazu — dann sind es Abrechnungsdaten und die Frage ist nicht mehr aufschiebbar |
| **Unit-Namensraum (§4)** | `kirla-api.service` + `kirla-labs-api.slice` — **Produkt**, nicht Betrieb. Steht **nicht** in `/var/log/kirla-setup/07-units-quelle.txt` und ist daher von der Maskierung aus 7.6 nicht erfasst. Sie ist auch nicht gewollt: der Dienst soll laufen. Wer je über `kirla-*` maskiert, nimmt ihn mit — §4 verbietet den Glob aus genau diesem Grund |
| **Owner** | Betreiber (t.stams) |

**Was dieser Dienst NICHT durchreicht, obwohl er alles unter `/v1/` weiterleitet:**

* **`GET /v1/key`** — klabs prüft damit den anbieterseitigen Ausgabendeckel. Durchgereicht
  sähe OpenRouter den **Hauptschlüssel** und meldete den Zustand des **Betreiberkontos**:
  Verbrauch, Limit und Rest *aller* Kunden. Ausgespart, antwortet `501`. Ab Schritt 2 liefert
  er den Guthabentopf des jeweiligen Schlüssels.
* **`GET /v1/route`** — die Auskunft über Ablage und Aufbewahrung, kommt mit Schritt 4.

**Offen und dem Betreiber gehörig:** die weiter oben genannte Regel verlangt für jeden neuen
Dienst eine Zeile in dieser Registry. Diese Zeile steht hier auf dem Host — die Kopfzeile
nennt aber `docs/server/SETUP.md` als Quelle der Wahrheit. Sie ist dorthin nachzuziehen,
sonst ist sie derselbe Nur-auf-dem-Host-Zustand, den Befund B-99 bei `kirla.ai.conf`
aufgedeckt hat.

---
