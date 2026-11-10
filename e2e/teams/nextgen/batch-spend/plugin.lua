-- An eligibility condition written in Lua against v2.0.0 of the rewards contracts: it decides on a batch of
-- subjects at once (Decide), each qualifying with at least 50.00 of yearly spend. Its types are plg.d.lua's (plg
-- types writes them).
local minimumCents = 5000

---@class BatchSpend: eligibility.Condition
local Condition = {}
Condition.__index = Condition

function Condition:Decide(subjects)
  local decisions = json.array()
  for i, subject in ipairs(subjects) do
    local spent = subject.user.spendCents
    if spent >= minimumCents then
      decisions[i] = { eligible = true, reason = string.format("spent %d cents", spent) }
    else
      decisions[i] = { eligible = false, reason = string.format("spent %d cents, under %d", spent, minimumCents) }
    end
  end
  return decisions
end

-- Validate rejects the condition when the platform lets conditions require less spend than it does.
function Condition:Validate(limits)
  if minimumCents > limits.maxSpendCents then
    return { {
      severity = "error",
      code = "batch-spend.minimum",
      message = string.format("requires %d cents, over the %d cents limit", minimumCents, limits.maxSpendCents),
      field = "minimumCents",
    } }
  end
  return {}
end

function Create()
  return setmetatable({}, Condition)
end
