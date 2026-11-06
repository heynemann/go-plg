-- A pricing rule written in Lua: loyal customers get a percentage off, by the tier the service reports through
-- its host (plg.WithHost[pricing.Customers]): host.Tier is the Go method Customers.Tier.
local percent = require("tiers")

local Rule = {}
Rule.__index = Rule

function Rule:Discount(order)
  local tier = host.Tier(order.customer)
  local off = percent[tier]
  if not off then
    return { cents = 0, reason = "" }
  end
  local subtotal = 0
  for _, item in ipairs(order.items or {}) do
    subtotal = subtotal + item.quantity * item.unitCents
  end
  return { cents = subtotal * off // 100, reason = tier .. " customer: " .. off .. "% off" }
end

-- Validate tells plg whether every tier fits the service's limits (plg.WithValidation): plg calls it in a
-- throwaway instance before the rule goes live, and an "error" issue keeps it from loading.
function Rule:Validate(limits)
  local issues = {}
  for tier, off in pairs(percent) do
    if off > limits.maxPercent then
      issues[#issues + 1] = {
        severity = "error",
        code = "loyalty.percent",
        message = tier .. " grants " .. off .. "% off, over the " .. limits.maxPercent .. "% limit",
        field = "tiers." .. tier,
      }
    end
  end
  return issues
end

function Create()
  return setmetatable({}, Rule)
end
