-- An eligibility condition written in Lua: the insights team's loyalty rule. Gold members qualify with at least two
-- orders in the last year, silver members with at least 150.00 of spend, and no one else; the reason says which,
-- with the amounts. The team keeps the rule in Go, Lua and TypeScript (loyal-go, loyal-lua, loyal-ts), every port
-- held to the same test cases, and the platform's tests to the same decisions over every user.
-- Its types are plg.d.lua's (plg types writes them).

-- The yearly orders a gold member needs, and the yearly spend a silver member needs.
local minGoldOrders = 2
local minSilverCents = 15000

-- amount writes cents as a decimal amount: 15000 is 150.00.
local function amount(cents)
  return string.format("%d.%02d", cents // 100, cents % 100)
end

---@class Loyal: eligibility.Condition
local Condition = {}
Condition.__index = Condition

function Condition:Eligible(subject)
  local user = subject.user
  if user.tier == "gold" then
    if user.orders < minGoldOrders then
      return { eligible = false, reason = string.format("gold member with %d orders, under %d", user.orders, minGoldOrders) }
    end
    return { eligible = true, reason = string.format("gold member with %d orders", user.orders) }
  end
  if user.tier == "silver" then
    if user.spendCents < minSilverCents then
      return {
        eligible = false,
        reason = "silver member who spent " .. amount(user.spendCents) .. ", under " .. amount(minSilverCents),
      }
    end
    return { eligible = true, reason = "silver member who spent " .. amount(user.spendCents) }
  end
  return { eligible = false, reason = user.tier .. " members are not in the loyalty programme" }
end

-- Validate rejects the condition when the platform lets conditions require less spend than silver members need.
function Condition:Validate(limits)
  if minSilverCents > limits.maxSpendCents then
    return { {
      severity = "error",
      code = "loyal.spend",
      message = "requires " .. amount(minSilverCents) .. " of spend, over the limit of " .. amount(limits.maxSpendCents),
      field = "minSilverCents",
    } }
  end
  return {}
end

function Create()
  return setmetatable({}, Condition)
end
