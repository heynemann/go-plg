-- A pricing rule written in Lua: the service's gold customers get a fixed amount off, never more than the
-- order's subtotal. It asks the service for the customer's tier through its Host (host.Tier).
local cents = 300

---@class Rule: pricing.Rule
local Rule = {}
Rule.__index = Rule

function Rule:Discount(order)
  if host.Tier(order.customer) ~= "gold" then
    return { cents = 0, reason = "" }
  end
  local subtotal = 0
  for _, item in ipairs(order.items or {}) do
    subtotal = subtotal + item.quantity * item.unitCents
  end
  return { cents = math.min(cents, subtotal), reason = "gold customer: " .. cents .. " cents off" }
end

-- Validate rejects the rule when the service lets rules grant less than its fixed amount.
function Rule:Validate(limits)
  if cents > limits.maxCents then
    return { {
      severity = "error",
      code = "vip.cents",
      message = cents .. " cents off, over the " .. limits.maxCents .. " cents limit",
      field = "cents",
    } }
  end
  return {}
end

function Create()
  return setmetatable({}, Rule)
end
