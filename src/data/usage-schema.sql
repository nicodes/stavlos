
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
