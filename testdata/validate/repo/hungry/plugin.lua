-- A cohort provider whose Create holds 24 MiB: within Lua's default 64 MiB, over a 16 MiB limit.
local held

function Create()
  held = string.rep("x", 24 * 1024 * 1024)
  return {
    IsMember = function(self, user) return #held > 0 end,
    List = function(self, cursor) return { users = {}, next = "" } end,
  }
end
