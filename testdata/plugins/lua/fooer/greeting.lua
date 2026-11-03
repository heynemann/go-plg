-- greeting mirrors testcontract.Greeting.
return function(name)
  return "hello, " .. name:match("^%s*(.-)%s*$")
end
