-- Plugins, one row each, per registration (the name of the plg.Register call serving them), and their files.
--
-- revision changes, from a sequence shared by every plugin, whenever the plugin's language, entrypoint, memory
-- limit or any of its files change: it is the plugin's version, so plg reloads exactly the plugins that changed.
-- Toggling enabled does not change it; deleting and re-creating a plugin gives it a revision it never had.

CREATE SEQUENCE plg_revision;

CREATE TABLE plg_plugins (
    registration text NOT NULL CHECK (registration <> '' AND strpos(registration, '/') = 0),
    key text NOT NULL CHECK (key <> '' AND key NOT IN ('.', '..') AND strpos(key, '/') = 0),
    language text NOT NULL CHECK (language IN ('go', 'lua', 'typescript')),
    entrypoint text NOT NULL CHECK (entrypoint <> ''),
    memory_limit bigint NOT NULL DEFAULT 0 CHECK (memory_limit BETWEEN 0 AND 4294967296),
    enabled boolean NOT NULL DEFAULT true,
    revision bigint NOT NULL DEFAULT nextval('plg_revision'),
    PRIMARY KEY (registration, key)
);

CREATE TABLE plg_plugin_files (
    registration text NOT NULL,
    key text NOT NULL,
    path text NOT NULL CHECK (path <> ''),
    content bytea NOT NULL,
    PRIMARY KEY (registration, key, path),
    FOREIGN KEY (registration, key) REFERENCES plg_plugins (registration, key) ON DELETE CASCADE
);

CREATE FUNCTION plg_plugin_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.language, NEW.entrypoint, NEW.memory_limit) IS DISTINCT FROM
       (OLD.language, OLD.entrypoint, OLD.memory_limit) AND NEW.revision = OLD.revision THEN
        NEW.revision := nextval('plg_revision');
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER plg_plugin_changed BEFORE UPDATE ON plg_plugins
    FOR EACH ROW EXECUTE FUNCTION plg_plugin_changed();

CREATE FUNCTION plg_file_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        UPDATE plg_plugins SET revision = nextval('plg_revision')
        WHERE registration = OLD.registration AND key = OLD.key;
    END IF;
    IF TG_OP <> 'DELETE' THEN
        UPDATE plg_plugins SET revision = nextval('plg_revision')
        WHERE registration = NEW.registration AND key = NEW.key;
    END IF;
    RETURN NULL;
END
$$;

CREATE TRIGGER plg_file_changed AFTER INSERT OR UPDATE OR DELETE ON plg_plugin_files
    FOR EACH ROW EXECUTE FUNCTION plg_file_changed();
