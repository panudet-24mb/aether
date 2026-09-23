import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "Aether — Spatial IoT Platform",
  description: "Aether IoT monitoring with live Minew gateway data, temperature, humidity and connection status.",
  icons: {
    icon: "/favicon.svg",
    shortcut: "/favicon.svg",
  },
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html lang="th" className="dark">
      <body className="antialiased">{children}</body>
    </html>
  );
}
