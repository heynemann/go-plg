-- An eligibility condition written in Lua: the fraud team's screening of a member's order history. Users without
-- an order to screen do not qualify. Its screening data covers every country but Mexico, where it fails (the
-- call's error, which the platform counts against it) rather than guess. Its types are plg.d.lua's (plg types
-- writes them).
local unscreened = { MX = true }

---@class Screening: eligibility.Condition
local Condition = {}
Condition.__index = Condition

function Condition:Eligible(subject)
  local user = subject.user
  if unscreened[user.country] then
    error("no screening data for " .. user.country, 0)
  end
  if user.orders == 0 then
    return { eligible = false, reason = "no orders to screen" }
  end
  return { eligible = true, reason = string.format("screened %d orders", user.orders) }
end

-- Validate warns of each country the platform serves that the screening data does not cover: its users fail.
function Condition:Validate(limits)
  local issues = {}
  for _, country in ipairs(limits.countries or {}) do
    if unscreened[country] then
      issues[#issues + 1] = {
        severity = "warning",
        code = "screening.coverage",
        message = "no screening data for " .. country .. ": its users fail",
        field = "countries",
      }
    end
  end
  return issues
end

function Create()
  return setmetatable({}, Condition)
end
