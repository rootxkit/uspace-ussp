import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { HomePage } from "@/portal/HomePage";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("portal.title.home") };
}

export default function Page() {
  return <HomePage />;
}
