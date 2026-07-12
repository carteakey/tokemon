import type { Metadata } from "next";
import { headers } from "next/headers";
import "./globals.css";

export async function generateMetadata(): Promise<Metadata> {
  const incoming = await headers();
  const host = incoming.get("x-forwarded-host") ?? incoming.get("host") ?? "localhost:3000";
  const protocol = incoming.get("x-forwarded-proto") ?? (host.startsWith("localhost") ? "http" : "https");
  const base = new URL(`${protocol}://${host}`);

  return {
    metadataBase: base,
    title: "Tokemon — Your coding tokens are evolving",
    description: "A tiny, local-first token garden for your coding agents.",
    icons: { icon: "/tokemon/token-dex.png", shortcut: "/tokemon/token-dex.png" },
    openGraph: {
      title: "Tokemon — Your coding tokens are evolving",
      description: "A tiny, local-first token garden for your coding agents.",
      type: "website",
      images: [{ url: new URL("/og.png", base), width: 1729, height: 910, alt: "Tokemon — your coding tokens are evolving" }],
    },
    twitter: { card: "summary_large_image", images: [new URL("/og.png", base)] },
  };
}

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return <html lang="en"><body>{children}</body></html>;
}
