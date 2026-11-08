-- A cohort provider whose Create never returns: only a deadline stops it.
function Create()
  while true do end
end
