-- Required by the semantic cache and installed when the development database
-- is first created.
CREATE EXTENSION IF NOT EXISTS vector;

-- Available for operational SQL and future database-side fingerprints.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
