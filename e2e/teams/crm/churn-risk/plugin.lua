-- A cohort provider written in Lua: the users a CRM vendor scores as likely to churn. The vendor's API knows the
-- segment; this plugin asks it, with the address and bearer token the platform resolves as the secrets
-- vendor_api_url and vendor_api_token (plugin.yaml declares them), so they change with the environment, never
-- with the plugin. Its types are plg.d.lua's (plg types writes them).
local segment = "churn-risk"

---@class ChurnRisk: cohort.Provider
local Provider = {}
Provider.__index = Provider

-- escape percent-encodes text for a URL's path or query.
local function escape(text)
  return (string.gsub(text, "[^%w%-%._~]", function(c)
    return string.format("%%%02X", string.byte(c))
  end))
end

-- get asks the vendor's API for path; a response it cannot get is the call's error.
local function get(path)
  local response, reason = http.request({
    url = secrets.get("vendor_api_url") .. path,
    headers = { Authorization = "Bearer " .. secrets.get("vendor_api_token") },
  })
  if response == nil then
    error("the vendor API could not be reached: " .. reason)
  end
  return response
end

-- failed is the error of an answer the plugin does not expect, with the API's own (trimmed) words: plg replaces
-- any secret they echo with [secret <name>].
local function failed(response, what)
  error("the vendor API answered " .. response.status .. " for " .. what .. ": " .. string.match(response.body, "^%s*(.-)%s*$"))
end

-- IsMember asks the vendor whether user is in the segment: 200 says yes, 404 no.
function Provider:IsMember(user)
  local response = get("/segments/" .. segment .. "/members/" .. escape(user))
  if response.status == 404 then
    return false
  end
  if response.status ~= 200 then
    failed(response, user)
  end
  return true
end

-- List returns the vendor's page of the segment at cursor, which the vendor's next cursor continues.
function Provider:List(cursor)
  local response = get("/segments/" .. segment .. "/members?cursor=" .. escape(cursor))
  if response.status ~= 200 then
    failed(response, "the page at '" .. cursor .. "'")
  end
  local page = json.decode(response.body)
  return { users = page.users or json.array(), next = page.next or "" }
end

function Create()
  return setmetatable({}, Provider)
end
