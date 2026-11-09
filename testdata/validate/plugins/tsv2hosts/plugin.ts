// Create asks a Host for a tier, but the v2 contract has two interfaces that could be the Host: plg validate cannot
// tell which without -host.
declare const host: { Tier(user: string): Promise<string> };

export async function Create() {
  const tier = await host.Tier("admin");
  return { IsMember: async (user: string) => tier === "gold" };
}
