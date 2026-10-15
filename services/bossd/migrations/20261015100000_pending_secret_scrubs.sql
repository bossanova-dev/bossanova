-- +goose Up
CREATE TABLE pending_secret_scrubs (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    requested_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
-- Previously moved keys can remain on free pages or in old WAL frames.
INSERT INTO pending_secret_scrubs (id)
SELECT 1 WHERE EXISTS (SELECT 1 FROM repos WHERE has_keyring_secrets = 1);

-- +goose Down
DROP TABLE pending_secret_scrubs;
