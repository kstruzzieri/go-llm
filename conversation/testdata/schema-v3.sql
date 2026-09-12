CREATE TABLE conversation_schema_version (
    version INTEGER PRIMARY KEY,
    description TEXT NOT NULL,
    applied_at INTEGER NOT NULL
);

INSERT INTO conversation_schema_version (version, description, applied_at) VALUES
    (1, 'baseline conversations table', 1700000000001),
    (2, 'conversation search shadow table and FTS5 index', 1700000000002),
    (3, 'durable conversation summaries', 1700000000003);

CREATE TABLE conversations (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL DEFAULT '',
    messages TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    summary_content TEXT NOT NULL DEFAULT '',
    summary_message_count INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_conversations_updated_id
    ON conversations(updated_at DESC, id ASC);

CREATE TABLE conversation_search (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL DEFAULT '',
    body TEXT NOT NULL,
    message_count INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE VIRTUAL TABLE conversation_fts USING fts5(
    id UNINDEXED,
    title,
    body
);

INSERT INTO conversations (
    id,
    title,
    messages,
    created_at,
    updated_at,
    summary_content,
    summary_message_count
) VALUES (
    'workspace:schema-v3-fixture',
    'Prior schema fixture',
    '[{"role":"user","content":"Inspect the calibrationtoken file."},{"role":"assistant","content":"","tool_calls":[{"id":"call_fixture","type":"function","function":{"name":"read_file","arguments":{"path":"fixture.go"}}}]},{"role":"tool","content":"package fixture","tool_name":"read_file","tool_call_id":"call_fixture"}]',
    1700000000123,
    1700000010456,
    'Earlier messages established the fixture contract.',
    7
);

INSERT INTO conversation_search (
    id,
    title,
    body,
    message_count,
    created_at,
    updated_at
) VALUES (
    'workspace:schema-v3-fixture',
    'Prior schema fixture',
    'user: Inspect the calibrationtoken file.
assistant:  [{"id":"call_fixture","type":"function","function":{"name":"read_file","arguments":{"path":"fixture.go"}}}]
tool: package fixture read_file call_fixture
Earlier messages established the fixture contract.',
    3,
    1700000000123,
    1700000010456
);

INSERT INTO conversation_fts (id, title, body)
SELECT id, title, body FROM conversation_search;
