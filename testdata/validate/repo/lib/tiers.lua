-- What the repository's Lua plugins share.
local tiers = {}

function tiers.is_silver(tier)
  return tier == "silver"
end

return tiers
