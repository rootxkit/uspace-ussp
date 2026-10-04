import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { ConsolePolicyPage } from "@/console/PolicyPage";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("console.title.policy") };
}

export default function Page() {
  return <ConsolePolicyPage />;
}
