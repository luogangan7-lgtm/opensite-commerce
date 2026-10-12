/** Routes the shell; depends on typed messages and React Router; called by main. Production cannot import the gallery. */
import { lazy, Suspense } from 'react';
import { Route, Routes } from 'react-router';
import { t } from '@opensite/component-contracts';

const Gallery = import.meta.env.DEV || import.meta.env.MODE === 'gallery'
  ? lazy(() => import('./gallery'))
  : null;

export function App() {
  return <Suspense fallback={<p>{t('common.loading')}</p>}><Routes>
    <Route path="/" element={<main id="main"><p>{t('app.ready')}</p></main>} />
    {Gallery && <Route path="/_gallery" element={<Gallery />} />}
    <Route path="*" element={<main id="main"><p>{t('app.ready')}</p></main>} />
  </Routes></Suspense>;
}
