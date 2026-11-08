// Create asks the Host for a tier: plg validate has no Host to answer.
declare const host: { Tier(user: string): Promise<string> };

export async function Create() {
  const tier = await host.Tier("admin");
  return {
    IsMember: async (user: string) => tier === "gold",
    List: (cursor: string) => ({ users: [], next: "" }),
  };
}
