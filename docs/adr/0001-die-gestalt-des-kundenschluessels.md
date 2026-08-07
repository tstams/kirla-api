# ADR-0001: Der Kundenschlüssel trägt eine Kennung, und gehasht wird mit bcrypt

## Status

Angenommen (07.08.2026). Setzt den Bauauftrag `docs/kirla-api-handoff.md` §2 aus dem
klabs-Repositorium um und trifft die zwei Entscheidungen, die er offen lässt: die **Gestalt**
des Schlüssels und den **Kostenfaktor** des Hashverfahrens.

## Kontext

Der Auftrag legt fest: der Kunde bekommt `kirla_sk_<zufall>`, gespeichert wird nur der Hash
(`argon2id` oder `bcrypt`), das Präfix bleibt im Klartext, damit Scanner einen versehentlich
veröffentlichten Schlüssel finden. Zwei Dinge sagt er nicht.

**Erstens: woran erkennt die Prüfung, welcher Hash zu diesem Schlüssel gehört?** Ist der
Schlüssel reiner Zufall ohne Struktur, bleibt nur, jeden gespeicherten Hash durchzuprobieren
— also je Anfrage so viele bcrypt-Läufe, wie es Kunden gibt. Bei einem Testnutzer fällt das
nicht auf. Bei hundert Kunden ist es der mit Abstand teuerste Teil des heißen Weges, und
zwar genau in der Spitze, in der alle Instanzen gleichzeitig takten.

**Zweitens: wie teuer darf eine Prüfung sein?** Gemessen auf kirla-01:

| Verfahren | Kosten je Prüfung |
|---|---:|
| bcrypt, Kostenfaktor 12 | **268 ms** |
| SHA-256 | ~1 µs |

## Entscheidung

**Der Schlüssel hat die Gestalt `kirla_sk_<kennung:12><_><geheimnis:32>`**, beides base32 in
Kleinbuchstaben ohne Polsterung. Die Kennung ist 60 Bit und dient nur dem Nachschlagen; das
Geheimnis ist 160 Bit und wird gehasht. Damit kostet eine Prüfung **einen** Zugriff auf eine
Landkarte und **einen** bcrypt-Lauf, unabhängig von der Zahl der Kunden.

**Gehasht wird mit bcrypt, Kostenfaktor 12** — der Auftrag erlaubt argon2id oder bcrypt.
bcrypt braucht keinen nennenswerten Speicher (~4 KB gegen 64 MB je argon2id-Prüfung), und
Speicher ist die Größe, die unter gleichzeitiger Last umkippt statt langsamer zu werden.

**Eine gelungene Prüfung wird 5 Minuten lang gemerkt**, damit dieselbe Instanz nicht bei
jedem Zug erneut 268 ms zahlt. Gemerkt wird der SHA-256 des Klartextes, nie der Klartext:
ein Speicherabbild soll keine Schlüssel preisgeben. **Die Sperre wird vor der Bestätigung
geprüft** — andernfalls liefe ein gesperrter Schlüssel bis zum Ablauf des Fensters weiter,
also genau in den Minuten, in denen es bei einem Missbrauch darauf ankommt.

## Alternativen

**Reiner Zufall ohne Kennung, Prüfung durch Durchprobieren.** Einfacher zu erzeugen, aber
O(n) bcrypt-Läufe je Anfrage. Bei 100 Kunden wären das 27 Sekunden Rechenzeit für **eine**
Prüfung. Nicht tragbar.

**Reiner Zufall, indiziert über einen schnellen Hash.** Man legt SHA-256 des ganzen
Schlüssels als Suchindex ab und prüft dann bcrypt. Funktioniert — aber wer den schnellen
Hash ohnehin ablegt, hat die Ablage bereits auf SHA-256-Sicherheit gebracht, und der
bcrypt-Lauf daneben ist Zierde.

**argon2id statt bcrypt.** Gegen Sonderrechner (GPU, ASIC) das stärkere Verfahren. Der
Vorteil greift bei von Menschen gewählten Kennwörtern; gegen 160 Bit Zufall probiert weder
eine GPU noch sonst etwas.

## Folgen

**Der Preis, und er gehört hierher:**

* **Die Gestalt des Schlüssels ist eine Einbahnstraße.** Sie später zu ändern heißt, jedem
  Kunden einen neuen Schlüssel zu geben und jede `CEO_LLM_API_KEY`-Zeile anzufassen. Deshalb
  steht sie vor dem ersten Testnutzer fest und nicht danach.
* **268 ms je frischer Prüfung.** Nach einem Neustart melden sich alle Instanzen neu an; bei
  20 Instanzen sind das gut 5 Sekunden Rechenzeit, die sich über die verfügbaren Kerne
  verteilen. Sichtbar, aber einmalig.
* **Ein gesperrter Schlüssel wird erst beim nächsten Neuladen der Ablage wirklich gesperrt**
  (SIGHUP oder Neustart). Die Bestätigung hält ihn nicht auf, die alte Ablage schon. Ab
  Schritt 3 füllt die Datenbank dieselbe Landkarte und das Fenster wird zur Kulanzfrist.

**Was offen bleibt und dem Betreiber gehört:** Ein `kirla_sk_…` ist kein von einem Menschen
gewähltes Kennwort, sondern 160 Bit Zufall aus `crypto/rand`. Ein langsames Hashverfahren
schützt davor, dass jemand ein schwaches Kennwort durchprobiert — gegen 160 Bit probiert
niemand, auch nicht mit gestohlener Ablage. **SHA-256 wäre hier ebenso sicher und rund
250 000-mal billiger.** Der Auftrag nennt argon2id oder bcrypt, also steht bcrypt; die
Umstellung wäre eine Änderung an einer Stelle und **ohne Neuvergabe von Schlüsseln**
möglich, weil die Gestalt davon unberührt bleibt. Das ist bewusst die reversible Hälfte
dieser Entscheidung — im Gegensatz zur Gestalt.
