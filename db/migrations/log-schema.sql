
CREATE TABLE IF NOT EXISTS events (
  global  INTEGER PRIMARY KEY,
  channel TEXT    NOT NULL,
  seq     INTEGER NOT NULL,
  agent   TEXT    NOT NULL DEFAULT '',
  type    TEXT    NOT NULL,
  time    INTEGER NOT NULL,
  payload BLOB,
  UNIQUE(channel, seq)
);
CREATE TABLE IF NOT EXISTS channels (
  id       TEXT    PRIMARY KEY,
  name     TEXT    NOT NULL UNIQUE,
  dir      TEXT    NOT NULL,
  created  INTEGER NOT NULL,
  archived INTEGER NOT NULL DEFAULT 0,
  title    TEXT    NOT NULL DEFAULT '',
  last_seq INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS events_type_time ON events(type, time);
CREATE TABLE IF NOT EXISTS trust (
  dir  TEXT PRIMARY KEY,
  hash TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS usage (
  channel  TEXT    NOT NULL,
  agent    TEXT    NOT NULL DEFAULT '',
  provider TEXT    NOT NULL DEFAULT '',
  time     INTEGER NOT NULL,
  fresh    INTEGER NOT NULL DEFAULT 0, -- input tokens charged as new
  cached   INTEGER NOT NULL DEFAULT 0, -- input tokens served from the provider's cache
  output   INTEGER NOT NULL DEFAULT 0,
  cost     REAL    NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS usage_time ON usage(time);
CREATE INDEX IF NOT EXISTS usage_channel_time ON usage(channel, time);

PRAGMA user_version = 6;
