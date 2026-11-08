-- A rule that builds a string of n MiB: its instance's memory grows with n, under TightRule's 16 MiB.
local Hog = {}
Hog.__index = Hog

function Hog:Apply(n)
  return #string.rep(string.rep("x", 1024), n * 1024)
end

function Create()
  return setmetatable({}, Hog)
end
