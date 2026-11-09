-- An eligibility condition written in Lua: users in the countries the CRM team runs campaigns for qualify. Its
-- types are plg.d.lua's (plg types writes them).
local countries = { "BR", "PT" }

---@class Country: eligibility.Condition
local Condition = {}
Condition.__index = Condition

local targeted = {}
for _, country in ipairs(countries) do
  targeted[country] = true
end

function Condition:Eligible(subject)
  local country = subject.user.country
  if targeted[country] then
    return { eligible = true, reason = "lives in " .. country }
  end
  return { eligible = false, reason = "lives in " .. country .. ", not in " .. table.concat(countries, " or ") }
end

-- Validate rejects the condition when it targets a country the platform does not serve.
function Condition:Validate(limits)
  local served = {}
  for _, country in ipairs(limits.countries or {}) do
    served[country] = true
  end
  local issues = {}
  for _, country in ipairs(countries) do
    if not served[country] then
      issues[#issues + 1] = {
        severity = "error",
        code = "country.unserved",
        message = "targets " .. country .. ", which the platform does not serve",
        field = "countries",
      }
    end
  end
  return issues
end

function Create()
  return setmetatable({}, Condition)
end
