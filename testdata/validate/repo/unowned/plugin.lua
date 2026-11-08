-- A cohort provider whose plugin.yaml names no owner.
function Create()
  return {
    IsMember = function(self, user) return false end,
    List = function(self, cursor) return { users = {}, next = "" } end,
  }
end
