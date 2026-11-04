-- The Lua test plugin that calls back into its host, like testdata/plugins/go/hosted: host is a global.
local Hosted = {}
Hosted.__index = Hosted

function Hosted:Describe(id)
  local user = host.Lookup(id) -- a host error raises, and becomes this method's error
  user.name = "described " .. user.name
  return user
end

function Hosted:Relay(msg)
  local ok, err = pcall(host.Fail, msg)
  if ok then
    return ""
  end
  return err
end

function Hosted:Value()
  return host.Value()
end

function Hosted:Origin()
  return self.origin
end

function Hosted:Total(base, more)
  return host.Sum(base, more)
end

function Hosted:Keys(keys)
  return host.Keys(keys)
end

-- Create calls the host once, so host calls work while Create runs.
function Create()
  return setmetatable({ origin = host.Value() }, Hosted)
end
