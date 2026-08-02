-- Add AtomCode (`atomcode`) as a protocol family. It runs via the ACP
-- transport (`atomcode acp`, JSON-RPC 2.0 over stdio), mirroring the qwenpaw
-- family's ACP-based backend. atomcode's ACP server does not implement
-- session/load or session/set_model (实测 5.0.3), so the backend ignores resume
-- and binds the model at process start via ~/.atomcode/config.toml. NOT VALID
-- preserves the historical-row tolerance used by prior family additions.
ALTER TABLE runtime_profile DROP CONSTRAINT IF EXISTS runtime_profile_protocol_family_check;

ALTER TABLE runtime_profile ADD CONSTRAINT runtime_profile_protocol_family_check
    CHECK (protocol_family IN (
        'claude',
        'codebuddy',
        'codex',
        'copilot',
        'opencode',
        'openclaw',
        'hermes',
        'pi',
        'cursor',
        'kimi',
        'kiro',
        'antigravity',
        'qoder',
        'qoderclicn',
        'traecli',
        'deveco',
        'grok',
        'qwen',
        'qwenpaw',
        'atomcode'
    )) NOT VALID;
