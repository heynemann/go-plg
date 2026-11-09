-- A cohort provider of the users whose loyalty tier is silver, by the repository's shared lib.
local tiers = require("lib.tiers")

local Silver = {}
Silver.__index = Silver

function Silver:IsMember(user)
  return tiers.is_silver(host.Tier(user))
end

function Silver:List(cursor)
  return { users = {}, next = "" }
end

function Create()
  assert(tiers.is_silver("silver"), "the shared lib does not tell silver")
  return setmetatable({}, Silver)
end
