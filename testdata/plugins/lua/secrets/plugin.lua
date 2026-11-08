-- A test plugin reading the secrets its plugin.yaml declares with secrets.get, like testdata/plugins/go/secrets:
-- Foo(name) returns the secret name, Foo("@create") what Create read of the secret create, and Fail(cmd) leaks a
-- secret: print (Lua has no stderr: print writes to stdout), error (returns nil and the message), panic (raises
-- it with error(), Lua's panic), fetch.
local Secrets = {}
Secrets.__index = Secrets

function Secrets:Foo(name)
  if name == "@create" then
    return self.created
  end
  return secrets.get(name)
end

function Secrets:Count() return 0 end

function Secrets:Fail(cmd)
  local action, name, url = cmd:match("^(%S+) (%S+) ?(%S*)$")
  local value = secrets.get(name)
  if action == "print" then
    print("token " .. value)
    return
  elseif action == "error" then
    return nil, "rejected " .. value
  elseif action == "panic" then
    error("leaked " .. value)
  elseif action == "fetch" then
    local _, err = http.request { url = url .. "/" .. value }
    return nil, err
  end
  return nil, "unknown command " .. action
end

function Secrets:Boil(c) return { c >= 100, c * 9 / 5 + 32 } end

function Secrets:Scalars(...) return { ... } end

function Secrets:Spin(n) return n end

function Create()
  local ok, value = pcall(secrets.get, "create")
  if not ok then
    value = "error: " .. tostring(value)
  end
  return setmetatable({ created = value }, Secrets)
end
