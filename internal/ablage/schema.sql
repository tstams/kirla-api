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
