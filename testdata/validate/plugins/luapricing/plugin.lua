-- A pricing rule, like gopricing and tspricing: the three run the same test cases.
local Rule = {}
Rule.__index = Rule

function Rule:Quote(order)
  if #order.lines == 0 then
    return nil, "empty order"
  end
  local total = 0
  for _, line in ipairs(order.lines) do
    total = total + line.qty * line.cents
  end
  local quote = { total = total, discount = 0, reasons = {} }
  if host.Tier(order.customer) == "gold" then
    quote.discount = total // 10
    quote.reasons[#quote.reasons + 1] = "gold"
  end
  if order.gift ~= nil then
    quote.reasons[#quote.reasons + 1] = "gift"
  end
  quote.total = total - quote.discount
  return quote
end

function Rule:Split(cents, parts)
  if parts <= 0 then
    return nil, "no parts"
  end
  local split = {}
  for i = 1, parts do
    split[i] = cents // parts
  end
  return { split, cents % parts }
end

function Rule:Coupon(code)
  local url = "https://coupons.example.com/codes/" .. code
  if code:find("://", 1, true) then
    url = code
  end
  local resp, err = http.request { url = url }
  if not resp then
    return nil, err
  end
  if resp.status == 200 then
    return math.tointeger(tonumber(resp.body))
  elseif resp.status == 404 then
    return nil, "unknown coupon " .. code
  end
  return nil, "coupon service: " .. resp.status
end

function Rule:Spin(mode)
  if mode == "panic" then
    hoard = string.rep("x", 1 << 27) -- twice the memory limit at once: out of memory without spinning
  elseif mode == "loop" then
    while true do end
  end
end

function Rule:Validate(limits)
  if limits.maxPercent < 10 then
    return { {
      severity = "error", code = "gold.percent", field = "maxPercent",
      message = "the gold discount (10%) is over the limit (" .. limits.maxPercent .. "%)",
    } }
  end
  return nil
end

function Create()
  return setmetatable({}, Rule)
end
