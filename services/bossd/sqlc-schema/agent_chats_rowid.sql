-- SQLite exposes rowid implicitly. sqlc needs it in its analysis catalog to
-- generate the insertion sequence used by AgentChat.Seq. This file is analysis
-- input only: it is never applied to a database.
ALTER TABLE agent_chats ADD COLUMN rowid INTEGER NOT NULL;
