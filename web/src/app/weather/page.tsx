import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { WeatherPage } from "@/portal/WeatherPage";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("portal.title.weather") };
}

export default function Page() {
  return <WeatherPage />;
}
