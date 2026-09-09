package storage

const schema = `
CREATE TABLE IF NOT EXISTS schema_version(version INTEGER PRIMARY KEY);
INSERT OR IGNORE INTO schema_version VALUES(1);
CREATE TABLE IF NOT EXISTS events (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE,
 pubkey TEXT NOT NULL, created_at INTEGER NOT NULL, kind INTEGER NOT NULL,
 d TEXT NOT NULL DEFAULT '', expires INTEGER NOT NULL DEFAULT 0, raw TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS ev_time ON events(created_at DESC,id);
CREATE INDEX IF NOT EXISTS ev_pk_kind ON events(pubkey,kind,created_at DESC);
CREATE INDEX IF NOT EXISTS ev_kind ON events(kind,created_at DESC);
CREATE INDEX IF NOT EXISTS ev_expires ON events(expires) WHERE expires>0;
CREATE TABLE IF NOT EXISTS tags(event_id TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,name TEXT NOT NULL,value TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS tag_lookup ON tags(name,value,event_id);
CREATE INDEX IF NOT EXISTS tag_event ON tags(event_id);
CREATE VIRTUAL TABLE IF NOT EXISTS search USING fts5(content);
CREATE TRIGGER IF NOT EXISTS events_ad AFTER DELETE ON events BEGIN
 DELETE FROM search WHERE rowid=old.seq;
END;
CREATE TABLE IF NOT EXISTS vanished(pubkey TEXT PRIMARY KEY,until INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS deletions(author TEXT NOT NULL,target_type TEXT NOT NULL,target TEXT NOT NULL,until INTEGER NOT NULL,PRIMARY KEY(author,target_type,target));
CREATE TABLE IF NOT EXISTS hidden_events(id TEXT PRIMARY KEY,reason TEXT NOT NULL DEFAULT 'report');
CREATE TABLE IF NOT EXISTS pending_events(id TEXT PRIMARY KEY,reason TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS marmot_principals(event_id TEXT PRIMARY KEY REFERENCES events(id) ON DELETE CASCADE,pubkey TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS marmot_principal_pk ON marmot_principals(pubkey);
CREATE TABLE IF NOT EXISTS list_history(owner TEXT NOT NULL,kind INTEGER NOT NULL,d TEXT NOT NULL DEFAULT '',event_id TEXT NOT NULL,created_at INTEGER NOT NULL,saved_at INTEGER NOT NULL,expires INTEGER NOT NULL DEFAULT 0,raw TEXT NOT NULL,PRIMARY KEY(owner,kind,d,event_id));
CREATE INDEX IF NOT EXISTS list_history_owner ON list_history(owner,saved_at DESC);
CREATE TABLE IF NOT EXISTS wiki_revisions(event_id TEXT PRIMARY KEY,author TEXT NOT NULL,d TEXT NOT NULL,created_at INTEGER NOT NULL,raw TEXT NOT NULL,superseded_by TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS wiki_revisions_page ON wiki_revisions(d,created_at DESC);
CREATE INDEX IF NOT EXISTS wiki_revisions_author ON wiki_revisions(author,d,created_at DESC);
CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS work_intents (
 id TEXT PRIMARY KEY,kind TEXT NOT NULL,event_id TEXT NOT NULL,target TEXT NOT NULL,payload TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'pending',attempts INTEGER NOT NULL DEFAULT 0,next_at INTEGER NOT NULL,
 created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,last_error TEXT NOT NULL DEFAULT '',
 claim_token TEXT NOT NULL DEFAULT '',claim_until INTEGER NOT NULL DEFAULT 0,
 UNIQUE(kind,event_id,target)
);
CREATE INDEX IF NOT EXISTS work_due ON work_intents(state,next_at);
CREATE TABLE IF NOT EXISTS audit(id INTEGER PRIMARY KEY AUTOINCREMENT,at INTEGER NOT NULL,actor TEXT NOT NULL,action TEXT NOT NULL,detail TEXT NOT NULL);
`
