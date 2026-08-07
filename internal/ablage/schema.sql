-- Schema der kirla.ai-Zwischenstelle. Cluster `labs` (Port 5442), Datenbank `labs`,
-- Rolle `labs_app` -- so festgelegt in /etc/kirla/REGISTRY.md §2 (E-05).
--
-- WAS HIER NICHT STEHT: Anfrage- und Antwortinhalte. Der Auftrag §5 trennt ausdruecklich
-- zwischen KOPFDATEN (~200 Byte, gehoeren hierher und duerfen DAUERHAFT bleiben, weil sie
-- Abrechnung und Zeitreihe sind) und INHALTEN (gehoeren daneben und verfallen nach 30
-- Tagen). Die Inhalte kommen mit Schritt 4 und NICHT in diese Tabellen.

CREATE TABLE IF NOT EXISTS schluessel (
    kennung    text        PRIMARY KEY,
    hash       text        NOT NULL,
    nutzer     text        NOT NULL,
    gesperrt   boolean     NOT NULL DEFAULT false,
    angelegt   timestamptz NOT NULL DEFAULT now()
);

-- Ein Topf je Schluessel (Auftrag §2: "Je Schluessel ein eigener Guthabentopf").
--
-- Betraege stehen in NANO-DOLLAR als bigint, nicht als numeric und schon gar nicht als
-- double: OpenRouter meldet neun Nachkommastellen, und Geld in Gleitkomma driftet. Der
-- Grund steht ausfuehrlich in internal/geld/geld.go.
CREATE TABLE IF NOT EXISTS topf (
    kennung        text        PRIMARY KEY REFERENCES schluessel(kennung) ON DELETE CASCADE,
    guthaben_nano  bigint      NOT NULL DEFAULT 0,
    aktualisiert   timestamptz NOT NULL DEFAULT now()
);

-- Die Kopfdaten. Ein Satz je Aufruf, ~200 Byte, dauerhaft.
CREATE TABLE IF NOT EXISTS aufruf (
    anfrage_id        text        PRIMARY KEY,
    kennung           text        NOT NULL,
    nutzer            text        NOT NULL,
    modell            text,
    zeit              timestamptz NOT NULL,
    dauer_ms          integer     NOT NULL,
    status            integer     NOT NULL,
    stroemend         boolean     NOT NULL DEFAULT false,
    tokens_ein        integer,
    tokens_aus        integer,
    -- was OpenRouter berechnet hat
    einkauf_nano      bigint      NOT NULL DEFAULT 0,
    -- was dem Kunden berechnet wird (Einkauf + Aufschlag)
    verkauf_nano      bigint      NOT NULL DEFAULT 0,
    -- Woher die Zahl kommt. Der Auftrag §4 verlangt einen Boden, wenn OpenRouter keine
    -- Kosten meldet -- aber "kein Feld" und "Feld steht auf 0" sind NICHT dasselbe:
    -- ein kostenloses Modell meldet ausdruecklich 0, und dafuer einen Boden zu buchen
    -- hiesse, Gratismodelle in Rechnung zu stellen. Gemessen am 07.08.2026 an
    -- inclusionai/ling-3.0-tiny:free.
    --   'gemeldet' = usage.cost war da und groesser als null
    --   'frei'     = usage.cost war da und genau null
    --   'boden'    = usage.cost fehlte; es wurde der Boden gebucht
    kosten_quelle     text        NOT NULL DEFAULT 'gemeldet'
        CHECK (kosten_quelle IN ('gemeldet', 'frei', 'boden'))
);

-- Die Zeitreihe wird nach Schluessel und Zeit gelesen ("was hat dieser Kunde diesen Monat
-- verbraucht"), deshalb dieser Index und kein anderer.
CREATE INDEX IF NOT EXISTS aufruf_kennung_zeit ON aufruf (kennung, zeit DESC);

-- Fuer die Betreibersicht ueber alle Kunden hinweg.
CREATE INDEX IF NOT EXISTS aufruf_zeit ON aufruf (zeit DESC);

-- ── DIE INHALTE, GETRENNT UND MIT VERFALL (Auftrag §5, Schritt 4) ──────────────────────
--
-- "Getrennt halten: Kopfdaten gehoeren in die Datenbank und duerfen DAUERHAFT bleiben;
-- sie sind Abrechnung und Zeitreihe. Die Inhalte gehoeren daneben und verfallen."
--
-- Genau deshalb eine eigene Tabelle und nicht zwei Spalten mehr in `aufruf`: ein DELETE
-- auf `inhalt` laesst die Abrechnung unberuehrt, und niemand kann versehentlich die
-- Zeitreihe mitloeschen.
--
-- ABGELEGT WIRD IMMER GZIP UEBER KLARTEXT. Kam die Antwort schon gepackt (Go setzt
-- `Accept-Encoding: gzip` von selbst), wird sie vorher entpackt. Damit gilt fuer jeden Satz
-- dieselbe Regel -- `gunzip` und man liest, was auf der Leitung stand. Ein Feld, in dem mal
-- das eine und mal das andere steht, kostet in einem Jahr mehr als die paar Zyklen jetzt.
--
-- Die Rechnung aus §5: ~15 KB je Aufruf, ~10 000 Aufrufe/Tag -> ~150 MB/Tag, 30 Tage
-- Bestand ~4,5 GB roh, komprimiert ~600 MB. Der Cluster liegt auf einem eigenen
-- Datentraeger mit 364 GB frei (gemessen 07.08.2026) -- das traegt.
CREATE TABLE IF NOT EXISTS inhalt (
    anfrage_id  text        PRIMARY KEY,
    zeit        timestamptz NOT NULL,
    anfrage     bytea,
    antwort     bytea,
    -- Content-Encoding, mit dem die Antwort ANKAM. Nur zur Diagnose; abgelegt ist sie
    -- entpackt und neu gepackt.
    kam_als     text,
    -- Wurde beim Mitschneiden gekuerzt, weil der Koerper die Grenze ueberschritt?
    gekuerzt    boolean     NOT NULL DEFAULT false
);

-- Der Aufraeumer laeuft nach Zeit. Ohne diesen Index waere er ein Tabellenscan ueber
-- 300 000 Saetze, jeden Tag.
CREATE INDEX IF NOT EXISTS inhalt_zeit ON inhalt (zeit);
