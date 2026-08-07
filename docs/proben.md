# Proben am laufenden Dienst

Was hier steht, ist an der echten Anlage gemessen worden und nicht im Test nachgestellt.
Ein Testlauf beweist die Logik; diese Proben beweisen den Dienst.

---

## Kulanzfenster und doppelte Buchführung (07.08.2026, 23:34–23:37)

**Aufbau:** `systemctl stop postgresql@16-labs` bei laufendem `kirla-api`. Der Cluster
`chronicle` blieb dabei unberührt.

**Ausgangsstand:** Topf `gddbs3fqr2oc` = 9 464 167 841 nano, Nachbuch leer.

### Während des Ausfalls

| Aufruf | HTTP | `/v1/key` → `limit_remaining` | `usage` |
|---|---|---:|---:|
| 1 | `200` | 9,464141621 | 0,000026220 |
| 2 | `200` | 9,464115401 | 0,000052440 |
| 3 | `200` | 9,464089181 | 0,000078660 |

Zwei Dinge stehen in dieser Tabelle:

* **Der Dienst antwortet weiter**, obwohl die Datenbank weg ist — das ist die Zusage aus
  ADR-0016.
* **Der Rest sinkt mit jedem Aufruf.** Ohne die doppelte Buchführung (Betreiberentscheidung
  07.08.) hätten alle drei Zeilen unverändert 9,464167841 gemeldet, und klabs hätte bis zu
  24 Stunden gegen einen Deckel gerechnet, den es nicht mehr gibt.

Im Nachbuch lagen danach **3 Sätze**.

### Nach `systemctl start`

```
INFO  nachbuch abgearbeitet    saetze=3
INFO  datenbank wieder da      gescheiterte_versuche=1
```

| | |
|---|---:|
| Topf in der Datenbank | **9 464 089 181** nano |
| 10 USD minus Summe aller Verkäufe | **9 464 089 181** nano |
| `/v1/key` danach: `usage` | 0 |

**Der Betrag wurde genau einmal abgezogen.** Und die stärkere Aussage steckt im Vergleich
mit der Tabelle oben: der Rest, den `/v1/key` *während* des Ausfalls meldete, ist auf die
Nanostelle derselbe, auf dem die Datenbank hinterher steht. Die Auskunft im Fenster war
also nicht bloß eine Schätzung, sondern bereits die Wahrheit.

Der doppelte Bestand — im Arbeitsspeicher als `verbraucht`, im Nachbuch als Satz — wird
über `Abgeglichen()` aufgelöst, das der Nachbuch-Lauf für jeden Stapel ruft, der wirklich
in der Datenbank steht. Zwei Tests in `internal/topf` halten beide Hälften fest, einer
davon prüft ausdrücklich, dass ohne diesen Rückruf **doppelt** abgezogen würde.

### Gedämpftes Protokoll

Ein früherer Lauf schrieb bei jedem Abgleichtakt (30 s) eine ERROR-Zeile mit dem
vollständigen Verbindungsfehler — über ein volles Fenster wären das 2880 Zeilen, auf einem
Host, dessen `kirla-ops-alert`-Units auf Fehler schauen. Gemessen nach der Dämpfung:
**1 Zeile in 70 Sekunden** statt 2–3, plus je eine Zeile beim Übergang ins Abgelaufene und
bei der Wiederkehr.
