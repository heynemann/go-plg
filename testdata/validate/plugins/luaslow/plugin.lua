-- A rule whose Create counts for many seconds: far past TightRule's 300ms timeout.
local Slow = {}
Slow.__index = Slow

function Slow:Apply(n)
  return n
end

function Create()
  local count = 0
  for i = 1, 3e9 do
    count = count + 1
  end
  return setmetatable({ count = count }, Slow)
end
