import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { ConsoleDssPage } from "@/console/DssPage";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("console.title.dss") };
}

export default function Page() {
  return <ConsoleDssPage />;
}
