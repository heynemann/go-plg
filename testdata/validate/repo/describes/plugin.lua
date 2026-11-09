-- A cohort provider implementing Describe, which only the local checkout of the contract (testdata/validate/
-- checkout/rewards) declares.
function Create()
  return {
    IsMember = function(self, user) return false end,
    List = function(self, cursor) return { users = {}, next = "" } end,
    Describe = function(self) return "nobody" end,
  }
end
