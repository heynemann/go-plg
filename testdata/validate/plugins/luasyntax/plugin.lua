-- A cohort provider whose entrypoint compiles and loads, but whose lib/tiers.lua, required only by List, does not
-- compile. Its long string holds what closes the shorter ones, which the syntax check embeds it past.
local Gold = {}
Gold.__index = Gold

local banner = [==[ ]] ]=] closes nothing here ]==]

function Gold:IsMember(user)
  return host.Tier(user) == "gold"
end

function Gold:List(cursor)
  local tiers = require("lib.tiers")
  return { users = tiers.gold(), next = banner }
end

function Create()
  return setmetatable({}, Gold)
end
