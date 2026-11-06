-- A plugin's manifest becomes one of its files, plugin.yaml, read by plg.ParseManifest like the files of any
-- storage, instead of the language, entrypoint and memory_limit columns. Each existing plugin gets a plugin.yaml
-- holding its columns' values (a JSON string is a YAML double-quoted scalar) unless it already has a file of that
-- name; the columns, and the trigger giving a new revision when they changed, go. From now on revision changes
-- whenever any of a plugin's files change, its plugin.yaml included.

INSERT INTO plg_plugin_files (registration, key, path, content)
SELECT registration, key, 'plugin.yaml', convert_to(
        'language: ' || language || E'\n' ||
        'entrypoint: ' || to_json(entrypoint)::text || E'\n' ||
        CASE WHEN memory_limit > 0 THEN 'memoryLimit: ' || memory_limit || E'\n' ELSE '' END,
        'UTF8')
FROM plg_plugins
ON CONFLICT (registration, key, path) DO NOTHING;

DROP TRIGGER plg_plugin_changed ON plg_plugins;
DROP FUNCTION plg_plugin_changed();

ALTER TABLE plg_plugins DROP COLUMN language, DROP COLUMN entrypoint, DROP COLUMN memory_limit;
