-- Air Defenders high-score schema (reference copy; actual schema is embedded in main.go).
--
-- SQLite, WAL mode. One row per submitted score. Append-only — no deletes.

CREATE TABLE IF NOT EXISTS scores (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    score      INTEGER NOT NULL,
    wave       INTEGER NOT NULL,
    created_at INTEGER NOT NULL,                 -- unix millis
    team       TEXT NOT NULL DEFAULT '',         -- MP auto-derived team label (e.g. "ALICE+2"); empty for SP / pre-migration
    members    TEXT NOT NULL DEFAULT '',         -- comma-joined contributor names (highest-rank first); MP only
    mode       TEXT NOT NULL DEFAULT 'sp'        -- 'sp' = single-player, 'mp' = multiplayer; splits the leaderboard
);
CREATE INDEX IF NOT EXISTS idx_scores_score      ON scores(score DESC);
CREATE INDEX IF NOT EXISTS idx_scores_created_at ON scores(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_scores_mode       ON scores(mode);
