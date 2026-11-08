-- A cohort provider of the users whose loyalty tier is gold.
local Gold = {}
Gold.__index = Gold

function Gold:IsMember(user)
  return host.Tier(user) == "gold"
end

function Gold:List(cursor)
  return { users = {}, next = "" }
end

function Create()
  return setmetatable({}, Gold)
end
