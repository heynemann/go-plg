-- A cohort provider written in Lua: the accounts piloting partner rewards, a fixed list that one page holds. Its
-- types are plg.d.lua's (plg types writes them).
local members = { "u000003", "u000012", "u000016" }

local pilots = {}
for _, member in ipairs(members) do
  pilots[member] = true
end

---@class Pilot: cohort.Provider
local Provider = {}
Provider.__index = Provider

function Provider:IsMember(user)
  return pilots[user] == true
end

-- List returns every pilot account on the first page; there is no other.
function Provider:List(cursor)
  if cursor ~= "" then
    error("the pilot cohort has one page, not one at '" .. cursor .. "'", 0)
  end
  return { users = members, next = "" }
end

function Create()
  return setmetatable({}, Provider)
end
